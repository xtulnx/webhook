package network

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Agent struct {
	cfg         *AgentConfig
	dialer      websocket.Dialer
	services    map[string]Service
	sem         chan struct{}
	wg          sync.WaitGroup
	hooks       func() []HookInfo
	downstream  *Hub
	webhook     http.Handler
	handlers    map[string]http.Handler
	info        AgentInfo
	shareKey    []byte
	shareBasic  bool
	shareUpload bool
	shareLinkID *string
	shareNames  []string
	shareReady  func(string)
}

var errShareLinksDenied = errors.New("hub denied share links; enable allow_share_links for this node")
var errShareLinkIDUnsupported = errors.New("hub did not acknowledge the custom share link ID; upgrade the hub")
var errShareUploadUnsupported = errors.New("hub did not acknowledge share uploads; upgrade the hub")
var errShareLinkConflict = errors.New("hub could not reserve the share link ID; choose another ID or wait for the previous registration to expire")

type AgentOptions struct {
	Version        string
	Temporary      bool
	ExpiresAt      *time.Time
	Hooks          func() []HookInfo
	Downstream     *Hub
	WebhookHandler http.Handler
	Handlers       map[string]http.Handler
	ShareNames     []string
	ShareBasic     bool
	ShareUpload    bool
	ShareLinkID    *string
	OnShareReady   func([]string)
}

func RunAgent(ctx context.Context, cfg *AgentConfig, options ...AgentOptions) error {
	a, err := newAgent(cfg)
	if err != nil {
		return err
	}
	if len(options) != 0 {
		a.hooks = options[0].Hooks
		a.downstream = options[0].Downstream
		a.webhook = options[0].WebhookHandler
		a.handlers = options[0].Handlers
		a.info.Version = options[0].Version
		a.info.Temporary = options[0].Temporary
		a.info.ExpiresAt = options[0].ExpiresAt
		a.shareLinkID = options[0].ShareLinkID
		if a.shareLinkID != nil && (len(options[0].ShareNames) == 0 || (*a.shareLinkID != "" && !validName(*a.shareLinkID))) {
			return errors.New("custom share link ID requires share names and must be empty or a valid URL segment")
		}
		if len(options[0].ShareNames) > 0 {
			if !a.info.Temporary || a.info.ExpiresAt == nil || a.handlers["files"] == nil || !a.services["files"].InProcess {
				return errors.New("share links require a temporary in-process files service")
			}
			a.shareKey = make([]byte, sha256.Size)
			a.shareBasic = options[0].ShareBasic
			a.shareUpload = options[0].ShareUpload
			if len(a.services) == 1 && len(cfg.Forwards) == 0 {
				a.shareNames = options[0].ShareNames
			}
			if _, err := cryptorand.Read(a.shareKey); err != nil {
				return fmt.Errorf("generate share key: %w", err)
			}
			if ready := options[0].OnShareReady; ready != nil {
				a.shareReady = func(base string) {
					ready(shareLinks(a.cfg.Server, base, a.cfg.ID, a.shareKey, a.shareBasic, a.shareLinkID, options[0].ShareNames))
				}
			}
		}
	}
	a.info.OS = runtime.GOOS
	a.info.Arch = runtime.GOARCH
	if a.info.Version == "" {
		a.info.Version = "unknown"
	}
	if cfg.WebhookOnlyViaHub && a.webhook == nil {
		return errors.New("webhook_only_via_hub requires an in-process webhook handler")
	}
	if a.hooks == nil && a.downstream != nil {
		if service, ok := a.services["webhook"]; ok && service.RelayTo != "" {
			id, _, _ := splitDestination(service.RelayTo)
			a.hooks = func() []HookInfo { return a.downstream.HooksForNode(id) }
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	var listeners []io.Closer
	var serveForwards []func()
	defer func() {
		cancel()
		for _, listener := range listeners {
			_ = listener.Close()
		}
		a.wg.Wait()
	}()
	for _, forward := range cfg.Forwards {
		forward := forward
		if forward.Protocol == "udp" {
			packet, err := net.ListenPacket("udp", forward.Listen)
			if err != nil {
				return fmt.Errorf("listen on UDP %s: %w", forward.Listen, err)
			}
			listeners = append(listeners, packet)
			serveForwards = append(serveForwards, func() {
				log.Printf("network UDP forward %s -> %s", packet.LocalAddr(), forward.To)
				a.receiveUDP(ctx, packet.(*net.UDPConn), forward.To)
			})
			continue
		}
		listener, err := net.Listen("tcp", forward.Listen)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", forward.Listen, err)
		}
		listeners = append(listeners, listener)
		serveForwards = append(serveForwards, func() {
			log.Printf("network forward %s -> %s", listener.Addr(), forward.To)
			a.accept(ctx, listener, forward)
		})
	}
	for _, serve := range serveForwards {
		a.wg.Add(1)
		go serve()
	}
	backoff := time.Second
	for ctx.Err() == nil {
		connectedAt := time.Now()
		err := a.control(ctx)
		if errors.Is(err, errShareLinksDenied) || errors.Is(err, errShareLinkIDUnsupported) ||
			errors.Is(err, errShareUploadUnsupported) || errors.Is(err, errShareLinkConflict) {
			return err
		}
		if time.Since(connectedAt) >= time.Minute {
			backoff = time.Second
		}
		if ctx.Err() != nil {
			break
		}
		log.Printf("network control disconnected: %v", err)
		jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
		select {
		case <-ctx.Done():
		case <-time.After(backoff + jitter):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
	return nil
}

func newAgent(cfg *AgentConfig) (*Agent, error) {
	a := &Agent{
		cfg:      cfg,
		services: make(map[string]Service),
		sem:      make(chan struct{}, cfg.MaxConnections),
		dialer: websocket.Dialer{
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: 10 * time.Second,
			TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
			ReadBufferSize:   4096,
			WriteBufferSize:  4096,
		},
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_file contains no certificates")
		}
		a.dialer.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	for _, service := range cfg.Services {
		a.services[service.Name] = service
	}
	return a, nil
}

func (a *Agent) openWebSocket(ctx context.Context, path, role, destination, session string) (*websocket.Conn, error) {
	nonce, err := newNonce()
	if err != nil {
		return nil, fmt.Errorf("generate connection nonce: %w", err)
	}
	head := http.Header{}
	head.Set(nodeHeader, a.cfg.ID)
	head.Set(nonceHeader, base64.RawURLEncoding.EncodeToString(nonce))
	if role != "" {
		head.Set(roleHeader, role)
	}
	if destination != "" {
		head.Set(toHeader, destination)
	}
	if session != "" {
		head.Set(sessionHeader, session)
	}
	key := sha256.Sum256([]byte(a.cfg.token))
	head.Set("Authorization", "Network-HMAC "+connectionProof(key, "webhook-network-agent-proof-v1", nonce, a.cfg.ID, path, role, destination, session))
	conn, response, err := a.dialer.DialContext(ctx, a.cfg.Server+path, head)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil && response != nil {
		return nil, fmt.Errorf("network hub returned HTTP %d: %w", response.StatusCode, err)
	}
	if err != nil {
		return nil, err
	}
	expected := connectionProof(key, "webhook-network-hub-proof-v1", nonce, a.cfg.ID, path, role, destination, session)
	if response == nil || subtle.ConstantTimeCompare([]byte(response.Header.Get(proofHeader)), []byte(expected)) != 1 {
		_ = conn.Close()
		return nil, errors.New("network hub authentication failed")
	}
	return conn, nil
}

func (a *Agent) control(ctx context.Context) error {
	conn, err := a.openWebSocket(ctx, transportPath(a.cfg.TransportPrefix, "/control"), "", "", "")
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	services := make([]string, 0, len(a.cfg.Services))
	var udpServices []string
	for _, service := range a.cfg.Services {
		services = append(services, service.Name)
		if service.Protocol == "udp" {
			udpServices = append(udpServices, service.Name)
		}
	}
	var inventory []HookInfo
	if a.hooks != nil {
		inventory = a.hooks()
	}
	if err := conn.WriteJSON(controlMessage{
		Info: &a.info, Type: "register", Services: services, UDPServices: udpServices, Hooks: inventory,
		ShareKey:    base64.RawURLEncoding.EncodeToString(a.shareKey),
		ShareBasic:  a.shareBasic,
		ShareUpload: a.shareUpload,
		ShareLinkID: a.shareLinkID,
		ShareNames:  a.shareNames,
	}); err != nil {
		return err
	}
	conn.SetReadLimit(controlLimit)
	if len(a.shareKey) > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(openTimeout))
		var response controlMessage
		if err := conn.ReadJSON(&response); err != nil {
			return fmt.Errorf("hub did not acknowledge share links: %w", err)
		}
		if response.Type == "share_links_conflict" {
			return errShareLinkConflict
		}
		if response.Type != "share_links_ready" || response.ShareBasic != a.shareBasic {
			return errShareLinksDenied
		}
		if a.shareLinkID != nil && (response.ShareLinkID == nil || *response.ShareLinkID != *a.shareLinkID) {
			return errShareLinkIDUnsupported
		}
		if a.shareUpload && !response.ShareUpload {
			return errShareUploadUnsupported
		}
		if a.shareReady != nil {
			a.shareReady(response.ShareBase)
			a.shareReady = nil
		}
	}
	if a.hooks != nil {
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			last := inventory
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					next := a.hooks()
					if reflect.DeepEqual(next, last) {
						continue
					}
					_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
					if err := conn.WriteJSON(controlMessage{Type: "inventory", Hooks: next}); err != nil {
						_ = conn.Close()
						return
					}
					last = next
				}
			}
		}()
	}
	log.Printf("network node %s connected to hub", a.cfg.ID)
	_ = conn.SetReadDeadline(time.Now().Add(65 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(65 * time.Second))
	})
	go pingLoop(conn, stop)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var message controlMessage
		if err := json.Unmarshal(data, &message); err != nil || (message.Type != "open" && message.Type != "open_udp") || message.Session == "" || !validName(message.Service) {
			return errors.New("invalid control message")
		}
		target, exists := a.services[message.Service]
		if !exists || (target.Protocol == "udp") != (message.Type == "open_udp") {
			continue
		}
		if !a.acquire() {
			continue
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			defer a.release()
			a.handleTarget(ctx, message.Session, target, message.Hops)
		}()
	}
}

func (a *Agent) acquire() bool {
	select {
	case a.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *Agent) release() { <-a.sem }

func (a *Agent) accept(ctx context.Context, listener net.Listener, forward Forward) {
	defer a.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if !a.acquire() {
			_ = conn.Close()
			continue
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			defer a.release()
			a.handleSource(ctx, conn, forward.To)
		}()
	}
}

func (a *Agent) handleSource(ctx context.Context, conn net.Conn, destination string) {
	defer conn.Close()
	ws, err := a.openWebSocket(ctx, transportPath(a.cfg.TransportPrefix, "/data"), "source", destination, "")
	if err != nil {
		log.Printf("network forward %s failed: %v", destination, err)
		return
	}
	a.bridge(ctx, conn, ws)
}

func (a *Agent) handleTarget(ctx context.Context, session string, service Service, hops int) {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	var conn net.Conn
	var err error
	if service.Protocol == "udp" {
		conn, err = (&net.Dialer{}).DialContext(dialCtx, "udp", service.Target)
	} else if service.RelayTo != "" {
		if a.downstream == nil || hops <= 1 {
			cancel()
			return
		}
		id, name, _ := splitDestination(service.RelayTo)
		conn, err = a.downstream.openPipe(dialCtx, id, name, hops-1)
	} else if handler := a.handlers[service.Name]; handler != nil {
		conn = serveWebhookInProcess(handler)
	} else if service.Name == "webhook" && service.Target == "" && a.webhook != nil {
		conn = serveWebhookInProcess(a.webhook)
	} else {
		conn, err = (&net.Dialer{}).DialContext(dialCtx, "tcp", service.Target)
	}
	cancel()
	if err != nil {
		log.Printf("network target %s failed: %v", service.Name, err)
		return
	}
	defer conn.Close()
	ws, err := a.openWebSocket(ctx, transportPath(a.cfg.TransportPrefix, "/data"), "target", "", session)
	if err != nil {
		log.Printf("network target tunnel failed: %v", err)
		return
	}
	if service.Protocol == "udp" {
		bridgeUDP(ctx, conn, ws)
		return
	}
	a.bridge(ctx, conn, ws)
}

func (a *Agent) bridge(ctx context.Context, conn net.Conn, ws *websocket.Conn) {
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
			_ = ws.Close()
		case <-stop:
		}
	}()
	bridgeTCP(conn, ws)
	close(stop)
}
