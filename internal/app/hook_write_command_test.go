package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xtulnx/webhook/internal/hook"
)

func TestHookFileManagementTransactions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.yaml")
	cfg := filepath.Join(dir, "webhook.yaml")
	input := filepath.Join(dir, "input.txt")
	original := []byte("# preserve in backup\n- id: first\n  execute-command: before\n")
	for p, data := range map[string][]byte{path: original, cfg: []byte("hooks: [hooks.yaml]\n"), input: []byte("id: second\nexecute-command: after\n")} {
		if e := os.WriteFile(p, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	run := func(action string, extra ...string) (int, map[string]interface{}, string) {
		t.Helper()
		var out, errs bytes.Buffer
		args := append([]string{"--config", cfg, "--file", path, "--working-directory", dir}, extra...)
		code := runHookWriteCommand(action, args, &out, &errs)
		var r map[string]interface{}
		if out.Len() > 0 {
			if e := json.Unmarshal(out.Bytes(), &r); e != nil {
				t.Fatal(e)
			}
		}
		return code, r, errs.String()
	}
	code, r, msg := run("add", "--from", input, "--dry-run")
	if code != 0 || r["saved"] != false {
		t.Fatalf("%d %v %s", code, r, msg)
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 3 {
		t.Fatalf("dry-run created files: %v", entries)
	}
	code, r, msg = run("add", "--from", input)
	if code != 0 || r["saved"] != true {
		t.Fatalf("%d %v %s", code, r, msg)
	}
	backup := r["backup"].(string)
	raw, e := os.ReadFile(backup)
	if e != nil || !bytes.Equal(raw, original) {
		t.Fatal("backup does not preserve exact bytes", e)
	}
	if isHooksConfigFile(backup) {
		t.Fatal("backup would be discovered as configuration")
	}
	info, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("permissions changed")
	}
	if code, _, _ := run("add", "--from", input); code != 1 {
		t.Fatal("duplicate add accepted")
	}
	if e := os.WriteFile(input, []byte("id: second\nexecute-command: updated\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if code, _, msg := run("update", "--id", "second", "--from", input); code != 0 {
		t.Fatal(code, msg)
	}
	if code, _, msg := run("delete", "--id", "first"); code != 0 {
		t.Fatal(code, msg)
	}
	var hs hook.Hooks
	if e := hs.LoadFromFileStrict(path, false); e != nil || len(hs) != 1 || hs[0].ID != "second" || hs[0].ExecuteCommand != "updated" {
		t.Fatalf("%+v %v", hs, e)
	}
	unlock, e := lockHookDirectories([]string{path})
	if e != nil {
		t.Fatal(e)
	}
	if code, _, msg := run("delete", "--id", "second"); code != 1 || !strings.Contains(msg, "locked") {
		t.Fatal("lock conflict not rejected", code, msg)
	}
	unlock()
}

func TestAdminMutationReadsFreshDisk(t *testing.T) {
	cleanup := setupAdminTestState(t)
	defer cleanup()
	path := filepath.Join(t.TempDir(), "hooks.json")
	if e := writeHooksFile(path, hook.Hooks{{ID: "original"}}); e != nil {
		t.Fatal(e)
	}
	loadedHooksMu.Lock()
	loadedHooksFromFiles = map[string]hook.Hooks{path: {{ID: "original"}}}
	loadedHooksMu.Unlock()
	if e := writeHooksFile(path, hook.Hooks{{ID: "original"}, {ID: "external"}}); e != nil {
		t.Fatal(e)
	}
	if e := upsertHookInFile(path, "", hook.Hook{ID: "admin"}); e != nil {
		t.Fatal(e)
	}
	var hs hook.Hooks
	if e := hs.LoadFromFileStrict(path, false); e != nil {
		t.Fatal(e)
	}
	if len(hs) != 3 {
		t.Fatalf("concurrent disk edit lost: %+v", hs)
	}
}

func TestHookTransactionRejectsConcurrentExternalEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.yaml")
	if e := os.WriteFile(path, []byte("- id: original\n"), 0600); e != nil {
		t.Fatal(e)
	}
	snapshot, e := readHookSnapshot(path)
	if e != nil {
		t.Fatal(e)
	}
	changed := []byte("- id: external\n")
	if e = os.WriteFile(path, changed, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = commitHookSnapshot([]hookDiskSnapshot{snapshot}, path, hook.Hooks{{ID: "replacement"}}); e == nil {
		t.Fatal("concurrent edit not detected")
	}
	raw, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(raw, changed) {
		t.Fatal("external edit overwritten")
	}
}

func TestHookManagementRejectsUnsafeInputs(t *testing.T) {
	for _, tt := range []struct{ name, target, other, input, config string }{
		{"unknownOriginal", "- id: original\n  future-field: true\n", "", "id: new\n", "hooks: [hooks.yaml]\n"},
		{"unknownInput", "- id: original\n", "", "id: new\nfuture-field: true\n", "hooks: [hooks.yaml]\n"},
		{"crossFileID", "- id: original\n", "- id: new\n", "id: new\n", "hooks: [hooks.yaml, other.yaml]\n"},
		{"template", "- id: original\n", "", "id: new\n", "hooks: [hooks.yaml]\ntemplate: true\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range map[string]string{"hooks.yaml": tt.target, "other.yaml": tt.other, "input.txt": tt.input, "webhook.yaml": tt.config} {
				if e := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); e != nil {
					t.Fatal(e)
				}
			}
			var out, errs bytes.Buffer
			code := runHookWriteCommand("add", []string{"--config", filepath.Join(dir, "webhook.yaml"), "--file", filepath.Join(dir, "hooks.yaml"), "--working-directory", dir, "--from", filepath.Join(dir, "input.txt")}, &out, &errs)
			if code != 1 {
				t.Fatalf("%d %s%s", code, out.String(), errs.String())
			}
			raw, e := os.ReadFile(filepath.Join(dir, "hooks.yaml"))
			if e != nil || string(raw) != tt.target {
				t.Fatal("source changed on rejection")
			}
		})
	}
}

func TestHookWriterChildProcess(t *testing.T) {
	if os.Getenv("WEBHOOK_WRITE_CHILD") != "yes" {
		return
	}
	code := runHookWriteCommand("add", []string{"--config", os.Getenv("WEBHOOK_WRITE_CONFIG"), "--file", os.Getenv("WEBHOOK_WRITE_TARGET"), "--from", os.Getenv("WEBHOOK_WRITE_INPUT"), "--working-directory", os.Getenv("WEBHOOK_WRITE_DIR")}, os.Stdout, os.Stderr)
	os.Exit(code)
}

func TestSeparateProcessesDoNotLoseHookWrites(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "hooks.json")
	config := filepath.Join(dir, "config.yaml")
	if e := os.WriteFile(target, []byte("[{\"id\":\"original\"}]"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(config, []byte("hooks: [hooks.json]\n"), 0600); e != nil {
		t.Fatal(e)
	}
	binary, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	type result struct {
		output []byte
		err    error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		input := filepath.Join(dir, fmt.Sprintf("input-%d.txt", i))
		if e := os.WriteFile(input, []byte(fmt.Sprintf("id: child-%d\n", i)), 0600); e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(binary, "-test.run=^TestHookWriterChildProcess$")
		cmd.Env = append(os.Environ(), "WEBHOOK_WRITE_CHILD=yes", "WEBHOOK_WRITE_CONFIG="+config, "WEBHOOK_WRITE_TARGET="+target, "WEBHOOK_WRITE_INPUT="+input, "WEBHOOK_WRITE_DIR="+dir)
		go func() { out, e := cmd.CombinedOutput(); results <- result{out, e} }()
	}
	successes := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err == nil {
			successes++
		} else if !strings.Contains(string(r.output), "locked") {
			t.Fatalf("unexpected write failure: %v %s", r.err, r.output)
		}
	}
	if successes == 0 {
		t.Fatal("no writer succeeded")
	}
	var hs hook.Hooks
	if e := hs.LoadFromFileStrict(target, false); e != nil {
		t.Fatal(e)
	}
	if len(hs) != successes+1 {
		t.Fatalf("lost write: %d successful writers, hooks %+v", successes, hs)
	}
}
