package app

import (
	"bytes"
	"github.com/xtulnx/webhook/internal/hook"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHooksManagement(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hooks := write("hooks.yaml", "- id: deploy\n  execute-command: definitely-do-not-execute\n  trigger-rule:\n    match:\n      type: payload-hmac-sha256\n      secret: private-test-secret\n      parameter:\n        source: header\n        name: X-Signature\n")
	config := write("webhook.yaml", "hooks:\n  - hooks.yaml\n")
	for _, tt := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"list", []string{"list", "--file", hooks, "--json"}, 0, "deploy"},
		{"show", []string{"show", "--file", hooks, "--id", "deploy"}, 0, "[REDACTED]"},
		{"reveal", []string{"show", "--file", hooks, "--id", "deploy", "--show-secrets"}, 0, "private-test-secret"},
		{"config", []string{"validate", "--config", config, "--working-directory", dir}, 0, "校验通过"},
		{"missing", []string{"show", "--file", hooks, "--id", "absent"}, 1, "未找到"},
		{"exclusive", []string{"list", "--file", hooks, "--config", config}, 2, "互斥"},
		{"help", []string{"list", "--help"}, 0, "用法"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out, errs bytes.Buffer
			code := runHooksCommand(tt.args, &out, &errs)
			if code != tt.code {
				t.Fatalf("code %d: %s%s", code, out.String(), errs.String())
			}
			combined := out.String() + errs.String()
			if !strings.Contains(combined, tt.want) {
				t.Fatalf("missing %q: %s", tt.want, combined)
			}
			if tt.name == "show" && strings.Contains(combined, "private-test-secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestHooksManagementAggregatesFailures(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("hooks: [a.yaml, b.yaml, missing.yaml]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.yaml", "b.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("- id: same\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errs bytes.Buffer
	if code := runHooksCommand([]string{"validate", "--config", cfg, "--working-directory", dir}, &out, &errs); code != 1 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"重复", "missing.yaml"} {
		if !strings.Contains(errs.String(), want) {
			t.Fatalf("missing %s: %s", want, errs.String())
		}
	}
	if out.Len() != 0 {
		t.Fatalf("partial success output: %s", out.String())
	}
}

func TestHooksManagementRejectsInvalidFields(t *testing.T) {
	for _, data := range []string{
		"- id: test\n  execute-comand: accidental-typo\n",
		"- id: test\n  id: duplicate\n",
		"- id: test\n  command-timeout: -1s\n",
		"- execute-command: echo\n",
	} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "hooks.yaml")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			if code := runHooksCommand([]string{"validate", "--file", path}, &out, &errs); code != 1 {
				t.Fatalf("code %d: %s%s", code, out.String(), errs.String())
			}
		})
	}
}

func TestManagementStrictParsingPreservesServerCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.yaml")
	if err := os.WriteFile(path, []byte("- id: hello\n  future-option: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var hooks hook.Hooks
	if err := hooks.LoadFromFile(path, false); err != nil {
		t.Fatalf("compatible server parser changed: %v", err)
	}
	if err := hooks.LoadFromFileStrict(path, false); err == nil {
		t.Fatal("management parser accepted unknown field")
	}
}
