package network

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParallelShareAgentsReconnect(t *testing.T) {
	token := strings.Repeat("s", 64)
	config, err := ParseConfig([]byte("mode: hub\nhub:\n  nodes:\n    - id: shared\n      token: "+token+"\n      services: [files]\n      allow_share_links: true\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := NewHub(config.Hub)
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	expiresAt, _ := ctx.Deadline()

	start := func(id string) string {
		t.Helper()
		cfg := &AgentConfig{
			ID: "shared", Token: token, Server: "ws" + strings.TrimPrefix(server.URL, "http"),
			Services: []Service{{Name: "files", InProcess: true}},
		}
		if err := ValidateAgent(cfg); err != nil {
			t.Fatal(err)
		}
		ready := make(chan string, 1)
		done := make(chan error, 1)
		go func() {
			done <- RunAgent(ctx, cfg, AgentOptions{
				Temporary: true, ExpiresAt: &expiresAt, ShareNames: []string{"pkg"}, ShareLinkID: &id,
				Handlers: map[string]http.Handler{"files": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/pkg" {
						http.NotFound(w, r)
						return
					}
					_, _ = io.WriteString(w, id)
				})},
				OnShareReady: func(links []string) { ready <- links[0] },
			})
		}()
		t.Cleanup(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("agent %s: %v", id, err)
			}
		})
		select {
		case link := <-ready:
			return link
		case <-ctx.Done():
			t.Fatalf("share %s did not register", id)
			return ""
		}
	}
	laptop, office := start("laptop"), start("office")
	route := func(id string) *controlConn {
		h.mu.Lock()
		defer h.mu.Unlock()
		control, _ := h.shareRouteLocked("shared", id+"/pkg")
		return control
	}
	old, other := route("laptop"), route("office")
	if old == nil || other == nil || old == other {
		t.Fatal("expected two live registrations")
	}
	if err := old.conn.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		if current := route("laptop"); current != nil && current != old {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("original agent did not reconnect")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if route("office") != other {
		t.Fatal("reconnect replaced the other agent")
	}
	for _, entry := range []struct{ link, want string }{{laptop, "laptop"}, {office, "office"}} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.link, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK || string(body) != entry.want {
			t.Errorf("unexpected response for %s: %d %q, %v", entry.want, res.StatusCode, body, err)
		}
	}
}

func TestShareRegistrationReconnect(t *testing.T) {
	now := time.Now()
	h := &Hub{shares: make(map[string][]*shareRegistration)}
	newControl := func(key, id string) *controlConn {
		return &controlConn{
			shareKey: []byte(strings.Repeat(key, 32)), shareLinkID: &id,
			shareIDs: map[string]bool{id: true}, shareExpiry: now.Add(5 * time.Minute),
			services: map[string]bool{"files": true}, shareNames: map[string]bool{"pkg": true},
		}
	}
	first, second := newControl("a", "laptop"), newControl("b", "office")
	if !h.registerShareLocked("shared", first, now) || !h.registerShareLocked("shared", second, now) {
		t.Fatal("distinct link IDs should register under the same node")
	}
	if h.serviceControlLocked("shared", "files") != nil {
		t.Fatal("ordinary files tunnel selected an ambiguous registration")
	}
	for _, c := range []*controlConn{first, second} {
		if got, path := h.shareRouteLocked("shared", *c.shareLinkID+"/pkg"); got != c || path != "pkg" {
			t.Fatalf("incorrect route for %s", *c.shareLinkID)
		}
	}
	randomControl := newControl("d", "")
	randomControl.shareLinkID = nil
	proof := shareProof(randomControl.shareKey, "pkg")
	randomControl.shareIDs = map[string]bool{proof: true}
	if !h.registerShareLocked("shared", randomControl, now) {
		t.Fatal("generated link ID could not register alongside custom IDs")
	}
	if got, path := h.shareRouteLocked("shared", proof+"/pkg"); got != randomControl || path != "pkg" {
		t.Fatal("generated link ID did not route to its registration")
	}
	for _, path := range []string{proof + "/other", "laptop/", "laptop/other"} {
		if got, _ := h.shareRouteLocked("shared", path); got != nil {
			t.Fatalf("unexpected route for %s", path)
		}
	}
	first.shareSlot.disconnected = now.Add(time.Second)
	if got, _ := h.shareRouteLocked("shared", "laptop/pkg"); got != nil {
		t.Fatal("disconnected registration served a request")
	}
	if h.registerShareLocked("shared", newControl("c", "laptop"), now.Add(2*time.Second)) {
		t.Fatal("another process took over a reserved link")
	}
	changed := newControl("a", "laptop")
	changed.shareNames["other"] = true
	if h.registerShareLocked("shared", changed, now.Add(2*time.Second)) {
		t.Fatal("reconnect changed the reserved set of share names")
	}
	reconnected := newControl("a", "laptop")
	if !h.registerShareLocked("shared", reconnected, now.Add(3*time.Second)) {
		t.Fatal("original process could not reconnect")
	}
	if h.controlActiveLocked("shared", first) || !h.controlActiveLocked("shared", reconnected) {
		t.Fatal("data connection should use the reconnected control only")
	}
	reconnected.shareSlot.disconnected = now.Add(4 * time.Second)
	if !h.registerShareLocked("shared", newControl("c", "laptop"), now.Add(4*time.Second+shareReconnectWindow)) {
		t.Fatal("reservation should be reusable after the reconnect window")
	}
}
