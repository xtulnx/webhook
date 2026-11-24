package network

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	udpSourceRole  = "source-udp"
	udpIdleTimeout = 30 * time.Second
	udpQueueSize   = 8
	udpMaxDatagram = 8 * 1024
)

func (a *Agent) receiveUDP(ctx context.Context, listener *net.UDPConn, destination string) {
	defer a.wg.Done()
	// Read the entire packet so an oversized datagram can be dropped without
	// forwarding a truncated payload or stopping the listener.
	buf := make([]byte, 65536)
	peers := make(map[netip.AddrPort]chan []byte)
	var mu sync.Mutex
	for {
		n, peer, err := listener.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if n > udpMaxDatagram {
			continue
		}
		mu.Lock()
		queue := peers[peer]
		if queue == nil && a.acquire() {
			queue = make(chan []byte, udpQueueSize)
			peers[peer] = queue
			a.wg.Add(1)
			go func(peer netip.AddrPort, queue chan []byte) {
				defer a.wg.Done()
				defer a.release()
				a.forwardUDP(ctx, listener, peer, destination, queue)
				mu.Lock()
				delete(peers, peer)
				mu.Unlock()
			}(peer, queue)
		}
		if queue != nil && len(queue) < cap(queue) {
			queue <- bytes.Clone(buf[:n])
		}
		mu.Unlock()
	}
}

func (a *Agent) forwardUDP(ctx context.Context, listener *net.UDPConn, peer netip.AddrPort, destination string, queue <-chan []byte) {
	ws, err := a.openWebSocket(ctx, transportPath(a.cfg.TransportPrefix, "/data"), udpSourceRole, destination, "")
	if err != nil {
		return
	}
	configureDataConn(ws)
	stop := make(chan struct{})
	defer close(stop)
	go pingLoop(ws, stop)

	replies := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		copyUDPReplies(ws, func(data []byte) error {
			if _, err := listener.WriteToUDPAddrPort(data, peer); err != nil {
				return err
			}
			select {
			case replies <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	defer func() {
		_ = ws.Close()
		<-done
	}()
	stopOnCancel := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stopOnCancel()

	idle := time.NewTimer(udpIdleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-idle.C:
			return
		case <-replies:
		case data := <-queue:
			if err := writeFrame(ws, websocket.BinaryMessage, data); err != nil {
				return
			}
		}
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(udpIdleTimeout)
	}
}

func bridgeUDP(ctx context.Context, conn net.Conn, ws *websocket.Conn) {
	configureDataConn(ws)
	stop := make(chan struct{})
	defer close(stop)
	go pingLoop(ws, stop)
	stopOnCancel := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		_ = ws.Close()
	})
	defer stopOnCancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ws.Close()
		buf := make([]byte, 65536)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if n <= udpMaxDatagram {
				if err := writeFrame(ws, websocket.BinaryMessage, buf[:n]); err != nil {
					return
				}
			}
		}
	}()
	copyUDPReplies(ws, func(data []byte) error {
		_, err := conn.Write(data)
		return err
	})
	_ = conn.Close()
	_ = ws.Close()
	<-done
}

func copyUDPReplies(ws *websocket.Conn, write func([]byte) error) {
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage || len(data) > udpMaxDatagram {
			return
		}
		if err := write(data); err != nil {
			return
		}
	}
}
