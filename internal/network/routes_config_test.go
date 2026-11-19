package network

import (
	"reflect"
	"strings"
	"testing"
)

func TestGroupedHookRoutes(t *testing.T) {
	t.Setenv("TEST_ROUTE_TOKEN", strings.Repeat("a", 64))
	const base = `mode: hub
hub:
  nodes:
    - {id: a1, token_env: TEST_ROUTE_TOKEN, services: [webhook]}
    - {id: a2, token_env: TEST_ROUTE_TOKEN, services: [webhook]}
    - {id: a20, token_env: TEST_ROUTE_TOKEN, services: [webhook]}
    - {id: files, token_env: TEST_ROUTE_TOKEN, services: [files]}
  agent_groups:
    release: ["re:a[12]"]
  hook_routes:
    - prefix: /ingress/
      agents: ["*"]
    - prefix: /ingress/team/
      groups: [release]
      agents: ["a?"]
      hooks: [dev/deploy/web]
    - path: /ingress/team/dev/deploy/web
      hook: exact/hook
      agents: [a20]
`
	cfg, err := ParseConfig([]byte(base), ".")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Hub.Routes[0].targets; !reflect.DeepEqual(got, []string{"a1", "a2", "a20"}) {
		t.Fatalf("wildcard targets = %v", got)
	}
	if got := cfg.Hub.Routes[1].targets; !reflect.DeepEqual(got, []string{"a1", "a2"}) {
		t.Fatalf("group targets = %v", got)
	}
	hub := NewHub(cfg.Hub)
	defer hub.Close()
	for _, test := range []struct {
		path  string
		mount string
		id    string
		ok    bool
	}{
		{"/ingress/team/dev/deploy/web", "/ingress/team/dev/deploy/web", "exact/hook", true},
		{"/ingress/team/dev/other", "/ingress/team", "dev/other", false},
		{"/ingress/dev/部署 v1", "/ingress", "dev/部署 v1", true},
		{"/ingress/dev/../web", "/ingress", "dev/../web", false},
		{"/ingress-more/dev", "", "", false},
	} {
		got, id, ok := hub.matchHook(test.path)
		if got.Mount() != test.mount || id != test.id || ok != test.ok {
			t.Errorf("match %q = %q, %q, %t", test.path, got.Mount(), id, ok)
		}
	}
	for _, test := range []struct{ old, replacement, message string }{
		{`"re:a[12]"`, `"re:("`, "invalid agent selector"},
		{`"re:a[12]"`, `"files"`, "matches no node offering webhook"},
		{"groups: [release]", "groups: [missing]", "unknown agent group"},
		{"prefix: /ingress/team/", "prefix: /ingress/", "duplicate hook route prefix"},
		{"prefix: /ingress/team/", "prefix: /__webhook_network/v1/share/nested/", "overlaps reserved path"},
	} {
		data := strings.Replace(base, test.old, test.replacement, 1)
		if _, err := ParseConfig([]byte(data), "."); err == nil || !strings.Contains(err.Error(), test.message) {
			t.Errorf("parse %q returned %v, want %q", test.replacement, err, test.message)
		}
	}
}
