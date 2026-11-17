package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRuntimeControlEndToEnd(t *testing.T) {
	binary, cleanup := buildWebhook(t)
	defer cleanup()
	// Keep the Unix socket path below the platform limit, including macOS.
	dir, err := os.MkdirTemp(".", "runtime-test-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	hooks := filepath.Join(dir, "hooks.yaml")
	cfg := filepath.Join(dir, "config.yaml")
	input := filepath.Join(dir, "new.txt")
	runtimeDir := filepath.Join(dir, "run-"+strings.Repeat("nested", 12))
	if err = os.WriteFile(hooks, []byte("- id: old\n  response-message: original\n  execute-command: absent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	config := fmt.Sprintf("ip: 127.0.0.1\nport: %d\nhooks: [%q]\nruntime-dir: %q\n", port, hooks, runtimeDir)
	if err = os.WriteFile(cfg, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(input, []byte("id: new\nresponse-message: added\nexecute-command: absent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", cfg)
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	url := fmt.Sprintf("http://127.0.0.1:%d/status", port)
	client := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		resp, e := client.Get(url)
		if e == nil {
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("server did not start")
	}
	run := func(args ...string) ([]byte, error) { return exec.Command(binary, args...).CombinedOutput() }
	raw, err := run("hooks", "add", "--config", cfg, "--file", hooks, "--from", input, "--reload", "required")
	if err != nil {
		t.Fatalf("save/refresh: %v %s", err, raw)
	}
	var result map[string]interface{}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result["runtimeConfirmed"] != true || result["reload"] != "applied" {
		t.Fatalf("%s", raw)
	}
	status := func() map[string]interface{} {
		t.Helper()
		resp, e := client.Get(url)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		var status map[string]interface{}
		if e = json.NewDecoder(resp.Body).Decode(&status); e != nil {
			t.Fatal(e)
		}
		return status
	}
	if count := status()["hooks"].(map[string]interface{})["count"]; count != float64(2) {
		t.Fatal("runtime not refreshed", count)
	}
	// A bad whole-file candidate must not remove the valid in-memory set.
	valid, err := os.ReadFile(hooks)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(hooks, []byte("- id: duplicate\n- id: duplicate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, e := run("hooks", "reload", "--config", cfg); e == nil {
		t.Fatalf("bad candidate accepted %s", out)
	}
	if count := status()["hooks"].(map[string]interface{})["count"]; count != float64(2) {
		t.Fatal("failed refresh changed runtime", count)
	}
	if err = os.WriteFile(hooks, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if out, e := run("hooks", "reload", "--config", cfg); e != nil {
		t.Fatalf("manual reload: %v %s", e, out)
	}
	// Changed startup configuration must be rejected before disk mutation.
	if err = os.WriteFile(cfg, []byte(config+"verbose: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(hooks)
	if out, e := run("hooks", "delete", "--config", cfg, "--file", hooks, "--id", "new", "--reload", "required"); e == nil {
		t.Fatalf("startup mismatch accepted %s", out)
	}
	after, _ := os.ReadFile(hooks)
	if !bytes.Equal(before, after) {
		t.Fatal("required preflight failure saved config")
	}
}

func TestRuntimeRequiredWithoutInstanceDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	hooks := filepath.Join(dir, "hooks.yaml")
	original := []byte("- id: old\n")
	if e := os.WriteFile(hooks, original, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(cfg, []byte("hooks: [hooks.yaml]\n"), 0600); e != nil {
		t.Fatal(e)
	}
	var out, errs bytes.Buffer
	code := runHookWriteCommand("delete", []string{"--config", cfg, "--file", hooks, "--working-directory", dir, "--id", "old", "--reload", "required"}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "未保存") {
		t.Fatalf("%d %s%s", code, out.String(), errs.String())
	}
	actual, _ := os.ReadFile(hooks)
	if !bytes.Equal(actual, original) {
		t.Fatal("required failure saved file")
	}
}

func TestSaveReportsUnconfirmedRefresh(t *testing.T) {
	dir, e := os.MkdirTemp(".", "control-failure-")
	if e != nil {
		t.Fatal(e)
	}
	dir, _ = filepath.Abs(dir)
	defer os.RemoveAll(dir)
	runtimeDir := filepath.Join(dir, "run")
	if e = secureRuntimeDirectory(runtimeDir, true); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(dir, "hooks.yaml")
	cfg := filepath.Join(dir, "config.yaml")
	input := filepath.Join(dir, "input.txt")
	if e = os.WriteFile(target, []byte("- id: old\n"), 0600); e != nil {
		t.Fatal(e)
	}
	raw := []byte(fmt.Sprintf("hooks: [%q]\nruntime-dir: %q\n", target, runtimeDir))
	if e = os.WriteFile(cfg, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(input, []byte("id: new\n"), 0600); e != nil {
		t.Fatal(e)
	}
	cwd, _ := os.Getwd()
	cwd, _ = canonicalRuntimePath(cwd)
	canonical, _ := canonicalRuntimePath(cfg)
	rec := runtimeRecord{ID: strings.Repeat("a", 32), PID: os.Getpid(), Directory: cwd, Config: canonical, ConfigHash: hashRuntimeBytes(raw)}
	listener, e := listenRuntimeControl(runtimeDir, rec.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	defer removeRuntimeEndpoint(runtimeDir, rec.ID)
	data, _ := json.Marshal(rec)
	if e = os.WriteFile(filepath.Join(runtimeDir, rec.ID+".instance"), data, 0600); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			var request controlRequest
			if e = json.NewDecoder(conn).Decode(&request); e == nil {
				response := controlResponse{Instance: rec, Files: []string{target}}
				if request.Operation == "reload" {
					response.Error = "injected reload failure"
				}
				_ = json.NewEncoder(conn).Encode(response)
			}
			conn.Close()
		}
	}()
	var out, errs bytes.Buffer
	code := runHookWriteCommand("add", []string{"--config", cfg, "--file", target, "--from", input, "--reload", "required"}, &out, &errs)
	if code != 3 {
		t.Fatalf("expected saved-but-unconfirmed: %d %s%s", code, out.String(), errs.String())
	}
	var result map[string]interface{}
	if e = json.Unmarshal(out.Bytes(), &result); e != nil {
		t.Fatal(e)
	}
	if result["saved"] != true || result["runtimeConfirmed"] != false {
		t.Fatal(result)
	}
	actual, _ := os.ReadFile(target)
	if !strings.Contains(string(actual), "new") {
		t.Fatal("saved file was rolled back")
	}
	<-done
}

func TestConfigInitIsExclusiveAndUsable(t *testing.T) {
	target := filepath.Join(t.TempDir(), "new")
	var out, errs bytes.Buffer
	if code := runConfigCommand([]string{"init", "--dir", target, "--dry-run"}, &out, &errs); code != 0 {
		t.Fatal(code, errs.String())
	}
	if _, e := os.Stat(target); !os.IsNotExist(e) {
		t.Fatal("preview created directory")
	}
	out.Reset()
	if code := runConfigCommand([]string{"init", "--dir", target}, &out, &errs); code != 0 {
		t.Fatal(code, errs.String())
	}
	config := filepath.Join(target, "webhook.yaml")
	files, e := readLocalHooks(localHookOptions{Config: config})
	if e != nil || len(files) != 1 || len(files[0].Hooks) != 0 {
		t.Fatalf("generated config unusable: %+v %v", files, e)
	}
	before, _ := os.ReadFile(config)
	if code := runConfigCommand([]string{"init", "--dir", target}, &out, &errs); code != 1 {
		t.Fatal("existing directory overwritten")
	}
	after, _ := os.ReadFile(config)
	if !bytes.Equal(before, after) {
		t.Fatal("configuration overwritten")
	}
}
