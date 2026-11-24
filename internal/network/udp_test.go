package network

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUDPForward(t *testing.T) {
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		buf := make([]byte, 65536)
		for {
			n, peer, err := target.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = target.WriteToUDP(buf[:n], peer)
		}
	}()

	entry, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	entryAddr := entry.LocalAddr().String()
	_ = entry.Close()

	tokenA, tokenB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	hubCfg := &HubConfig{Nodes: []NodePolicy{
		{ID: "a", Token: tokenA, Allow: []string{"b/dns"}},
		{ID: "b", Token: tokenB, Services: []string{"dns", "private"}},
	}}
	if err := validateHub(hubCfg, "."); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(hubCfg)
	server := httptest.NewServer(hub)
	defer server.Close()
	defer hub.Close()
	serverURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cfgA := &AgentConfig{ID: "a", Token: tokenA, Server: serverURL, Forwards: []Forward{
		{Listen: entryAddr, To: "b/dns", Protocol: "udp"},
	}}
	cfgB := &AgentConfig{ID: "b", Token: tokenB, Server: serverURL, Services: []Service{
		{Name: "dns", Target: target.LocalAddr().String(), Protocol: "udp"},
		{Name: "private", Target: target.LocalAddr().String(), Protocol: "udp"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 2)
	for _, cfg := range []*AgentConfig{cfgA, cfgB} {
		if err := ValidateAgent(cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range []*AgentConfig{cfgA, cfgB} {
		go func(cfg *AgentConfig) { done <- RunAgent(ctx, cfg) }(cfg)
	}
	defer func() {
		cancel()
		for i := 0; i < 2; i++ {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("agent: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("agent did not stop")
			}
		}
	}()

	for deadline := time.Now().Add(5 * time.Second); ; {
		hub.mu.Lock()
		ready := hub.controls["a"] != nil && hub.controls["b"] != nil && hub.controls["b"].udpServices["dns"]
		hub.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agents did not register")
		}
		time.Sleep(10 * time.Millisecond)
	}

	agent, err := newAgent(cfgA)
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []struct {
		role, to string
		status   int
	}{{"source", "b/dns", 503}, {udpSourceRole, "b/private", 403}} {
		ws, err := agent.openWebSocket(ctx, DataPath, denied.role, denied.to, "")
		if err == nil {
			_ = ws.Close()
		}
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", denied.status)) {
			t.Fatalf("%s %s: want HTTP %d, got %v", denied.role, denied.to, denied.status, err)
		}
	}

	var clients []net.Conn
	for i := 0; i < 2; i++ {
		client, err := net.Dial("udp", entryAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		clients = append(clients, client)
	}

	// An oversized packet must be dropped rather than split or truncated.
	if _, err := clients[0].Write(make([]byte, udpMaxDatagram+1)); err != nil {
		t.Fatal(err)
	}
	packets := [][][]byte{
		{[]byte("one"), {}, bytes.Repeat([]byte{1}, udpMaxDatagram)},
		{[]byte("two"), []byte("separate datagram")},
	}
	for i, client := range clients {
		for _, packet := range packets[i] {
			if _, err := client.Write(packet); err != nil {
				t.Fatal(err)
			}
		}
	}
	buf := make([]byte, udpMaxDatagram+1)
	for i, client := range clients {
		for _, packet := range packets[i] {
			n, err := client.Read(buf)
			if err != nil || !bytes.Equal(buf[:n], packet) {
				t.Fatalf("client %d: want %d bytes, got %d, error %v", i, len(packet), n, err)
			}
		}
	}
}
