package network

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const SharePath = "/__webhook_network/v1/share/"
const ShareDownloadHeader = "X-Webhook-Share-Download"
const ShareIndexHeader = "X-Webhook-Share-Index"
const ShareIndexCSP = "sandbox allow-downloads; default-src 'none'; base-uri 'none'; form-action 'none'"
const ShareBasicChallenge = `Basic realm="webhook share", charset="UTF-8"`

func shareProof(key []byte, name string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, name)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func shareLinkID(key []byte, name string, basic bool) string {
	if basic {
		// The short ID identifies the temporary session. The agent checks Basic Auth.
		return base64.RawURLEncoding.EncodeToString(key[:16])
	}
	return shareProof(key, name)
}

func shareLinks(server, base, node string, key []byte, basic bool, names []string) []string {
	if base == "" {
		base = strings.TrimSuffix(SharePath, "/")
	}
	if strings.HasPrefix(base, "/") {
		scheme, rest, _ := strings.Cut(server, "://")
		if scheme == "wss" {
			scheme = "https"
		} else {
			scheme = "http"
		}
		base = scheme + "://" + rest + base
	}
	links := make([]string, 0, len(names))
	for _, name := range names {
		links = append(links, base+"/"+node+"/"+shareLinkID(key, name, basic)+"/"+name)
	}
	return links
}

func (h *Hub) serveShare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", ShareIndexCSP)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	node, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, h.sharePath), "/")
	proof, filePath, _ := strings.Cut(rest, "/")
	name, _, _ := strings.Cut(filePath, "/")
	if !validName(node) || !validName(name) || (len(proof) != base64.RawURLEncoding.EncodedLen(sha256.Size) && len(proof) != base64.RawURLEncoding.EncodedLen(16)) {
		http.NotFound(w, r)
		return
	}
	h.mu.Lock()
	control := h.controls[node]
	h.mu.Unlock()
	now := time.Now()
	if control == nil || control.shareKey == nil || !now.Before(control.shareExpiry) ||
		!hmac.Equal([]byte(proof), []byte(shareLinkID(control.shareKey, name, control.shareBasic))) {
		http.NotFound(w, r)
		return
	}
	deadline := now.Add(h.requestTimeout)
	if control.shareExpiry.Before(deadline) {
		deadline = control.shareExpiry
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(deadline.Add(time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	out := &http.Request{
		Method: r.Method,
		URL:    &url.URL{Scheme: "http", Host: "network.internal", Path: "/" + filePath},
		Header: make(http.Header),
		Close:  true,
	}
	for _, name := range []string{"Range", "If-Range", "If-Modified-Since", "If-Unmodified-Since"} {
		for _, value := range r.Header.Values(name) {
			out.Header.Add(name, value)
		}
	}
	if control.shareBasic {
		out.Header.Set("Authorization", r.Header.Get("Authorization"))
	}
	out.Header.Set(ShareDownloadHeader, "1")
	transport := &http.Transport{
		DisableKeepAlives:  true,
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return h.openControlPipe(ctx, node, "files", maxHops, control)
		},
	}
	defer transport.CloseIdleConnections()
	response, err := transport.RoundTrip(out.WithContext(ctx))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			http.Error(w, "share download timed out", http.StatusGatewayTimeout)
		} else {
			http.Error(w, "share temporarily unavailable", http.StatusBadGateway)
		}
		return
	}
	defer response.Body.Close()
	for _, key := range []string{"Accept-Ranges", "Content-Length", "Content-Range", "Content-Type", "Last-Modified"} {
		if value := response.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	if control.shareBasic && response.StatusCode == http.StatusUnauthorized && response.Header.Get("WWW-Authenticate") == ShareBasicChallenge {
		w.Header().Set("WWW-Authenticate", response.Header.Get("WWW-Authenticate"))
	} else if response.StatusCode != http.StatusOK || response.Header.Get(ShareIndexHeader) != "1" {
		_, params, _ := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", params))
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
}
