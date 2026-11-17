package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtulnx/webhook/internal/hook"
)

func TestLocalHookHelper(t *testing.T) {
	if os.Getenv("WEBHOOK_TEST_CHILD") != "yes" {
		return
	}
	if os.Getenv("WEBHOOK_TEST_SLEEP") == "yes" {
		time.Sleep(5 * time.Second)
	}
	if path := os.Getenv("HOOK_FILE_ARTIFACT"); path != "" {
		raw, e := os.ReadFile(path)
		if e != nil {
			os.Exit(2)
		}
		fmt.Print(string(raw))
		_ = os.Remove(path)
	}
	os.Exit(0)
}

func TestLocalHookContext(t *testing.T) {
	dir := t.TempDir()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	h := hook.Hook{ID: "deploy", ExecuteCommand: exe, KeepFileEnvironment: true, PassArgumentsToCommand: []hook.Argument{{Source: hook.SourceString, Name: "-test.run=^TestLocalHookHelper$"}}, PassEnvironmentToCommand: []hook.Argument{{Source: hook.SourceString, Name: "yes", EnvName: "WEBHOOK_TEST_CHILD"}}, TriggerRule: &hook.Rules{Match: &hook.MatchRule{Type: hook.IPWhitelist, IPRange: "192.0.2.0/24"}}}
	path := filepath.Join(dir, "hooks.json")
	save := func() {
		t.Helper()
		data, _ := json.Marshal([]hook.Hook{h})
		if e := os.WriteFile(path, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	save()
	run := func(action string, args ...string) (int, testResult) {
		t.Helper()
		var out, errs bytes.Buffer
		all := append([]string{"--file", path, "--id", "deploy"}, args...)
		code := runHookTestCommand(action, all, &out, &errs)
		var r testResult
		if e := json.Unmarshal(out.Bytes(), &r); e != nil {
			t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errs.String())
		}
		return code, r
	}
	t.Run("assumed", func(t *testing.T) {
		code, r := run("explain")
		if code != 0 || !r.WouldTrigger || r.Executed || len(r.Assumptions) != 1 {
			t.Fatalf("%d %+v", code, r)
		}
	})
	t.Run("explicitDenied", func(t *testing.T) {
		code, r := run("test", "--remote-ip", "198.51.100.1", "--execute")
		if code != 4 || r.Executed {
			t.Fatalf("%d %+v", code, r)
		}
	})
	t.Run("strictMissing", func(t *testing.T) {
		code, r := run("explain", "--strict")
		if code == 0 || r.Executed {
			t.Fatalf("%d %+v", code, r)
		}
	})
	t.Run("upload", func(t *testing.T) {
		source := filepath.Join(dir, "package.zip")
		if e := os.WriteFile(source, []byte("artifact-data"), 0600); e != nil {
			t.Fatal(e)
		}
		code, r := run("test", "--uploaded-file", "artifact="+source, "--execute")
		if code != 0 || !r.Executed || r.Output != "artifact-data" {
			t.Fatalf("%d %+v", code, r)
		}
		raw, e := os.ReadFile(source)
		if e != nil || string(raw) != "artifact-data" {
			t.Fatal("source upload modified", e)
		}
	})
	t.Run("missingCommandInput", func(t *testing.T) {
		h.PassArgumentsToCommand = append(h.PassArgumentsToCommand, hook.Argument{Source: hook.SourceQuery, Name: "env"})
		save()
		code, r := run("test", "--execute")
		if code != 1 || r.Executed || !strings.Contains(r.Diagnostic, "缺少输入") {
			t.Fatalf("%d %+v", code, r)
		}
		h.PassArgumentsToCommand = h.PassArgumentsToCommand[:1]
		save()
	})
	t.Run("explicitEmpty", func(t *testing.T) {
		h.TriggerRule = &hook.Rules{Match: &hook.MatchRule{Type: hook.MatchValue, Value: "production", Parameter: hook.Argument{Source: hook.SourceQuery, Name: "env"}}}
		save()
		code, r := run("explain", "--query", "env=")
		if code != 4 || len(r.Assumptions) != 0 {
			t.Fatalf("%d %+v", code, r)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		h.TriggerRule = nil
		h.PassEnvironmentToCommand = append(h.PassEnvironmentToCommand, hook.Argument{Source: hook.SourceString, Name: "yes", EnvName: "WEBHOOK_TEST_SLEEP"})
		save()
		code, r := run("test", "--execute", "--timeout", "100ms")
		if code != 1 || !r.Executed || !r.TimedOut {
			t.Fatalf("%d %+v", code, r)
		}
	})
}

func TestAssistedRuleCombinations(t *testing.T) {
	missing := hook.Rules{Match: &hook.MatchRule{Type: hook.IPWhitelist, IPRange: "192.0.2.0/24"}}
	failed := hook.Rules{Match: &hook.MatchRule{Type: hook.MatchValue, Value: "expected", Parameter: hook.Argument{Source: hook.SourceString, Name: "actual"}}}
	req := &hook.Request{}
	for _, tt := range []struct {
		name string
		rule hook.Rules
		want bool
	}{
		{"orMissingCannotHideFailure", hook.Rules{Or: &hook.OrRule{missing, failed}}, false},
		{"andMissingCannotHideFailure", hook.Rules{And: &hook.AndRule{missing, failed}}, false},
		{"notMissingIsWaived", func() hook.Rules { n := hook.NotRule(missing); return hook.Rules{Not: &n} }(), true},
		{"emptyOrRemainsFalse", hook.Rules{Or: &hook.OrRule{}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var assumptions []string
			r, e := filterTestRules(&tt.rule, req, testContext{}, false, &assumptions, "rule")
			if e != nil {
				t.Fatal(e)
			}
			got := true
			if r != nil {
				got, e = r.Evaluate(req)
			}
			if e != nil || got != tt.want {
				t.Fatalf("got %t %v", got, e)
			}
		})
	}
}

func TestLocalContextFixture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	h := hook.Hook{ID: "fixture", ExecuteCommand: "unused", HTTPMethods: []string{"POST"}, PassArgumentsToCommand: []hook.Argument{{Source: hook.SourceQuery, Name: "env"}}}
	raw, _ := json.Marshal([]hook.Hook{h})
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	fixture := filepath.Join(dir, "context.yaml")
	if e := os.WriteFile(fixture, []byte("method: POST\nquery:\n  - name: env\n    value: secret-value\n"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, tt := range []struct {
		name  string
		extra []string
		code  int
	}{
		{"relativeFixture", []string{}, 0},
		{"contextFlagsConflict", []string{"--remote-ip", "192.0.2.1"}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			args := append([]string{"--file", path, "--id", "fixture", "--context", fixture}, tt.extra...)
			code := runHookTestCommand("explain", args, &out, &errs)
			if code != tt.code {
				t.Fatalf("code %d %s%s", code, out.String(), errs.String())
			}
			if strings.Contains(out.String(), "secret-value") {
				t.Fatal("value leaked")
			}
		})
	}
}

func TestLocalSignatureRequiresOriginalBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	h := hook.Hook{ID: "signed", ExecuteCommand: "unused", TriggerRule: &hook.Rules{Match: &hook.MatchRule{Type: hook.MatchHMACSHA256, Secret: "config-secret", Parameter: hook.Argument{Source: hook.SourceHeader, Name: "X-Signature"}}}}
	raw, _ := json.Marshal([]hook.Hook{h})
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	for _, tt := range []struct {
		name  string
		extra []string
		code  int
	}{
		{"missingSignatureAssumed", nil, 0},
		{"explicitSignatureWithoutBody", []string{"--header", "X-Signature=sensitive-signature"}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			args := append([]string{"--file", path, "--id", "signed"}, tt.extra...)
			code := runHookTestCommand("explain", args, &out, &errs)
			if code != tt.code {
				t.Fatalf("code %d %s%s", code, out.String(), errs.String())
			}
			for _, secret := range []string{"config-secret", "sensitive-signature"} {
				if strings.Contains(out.String()+errs.String(), secret) {
					t.Fatal("secret leaked")
				}
			}
		})
	}
}
