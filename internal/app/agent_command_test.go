package app

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xtulnx/webhook/internal/hook"
	"github.com/xtulnx/webhook/internal/network"
)

func TestTemporaryShares(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.war")
	if err := os.WriteFile(file, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	handler, err := newShareHandler([]string{"pkg=" + file, "dist=" + dir}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path string
		status       int
	}{{"GET", "/pkg", 200}, {"HEAD", "/pkg", 200}, {"GET", "/dist/app.war", 200}, {"GET", "/dist", 404}, {"GET", "/dist/../app.war", 404}, {"GET", "/dist/%2e%2e/app.war", 404}, {"POST", "/pkg", 405}} {
		r := httptest.NewRequest(test.method, test.path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s %s: %d", test.method, test.path, w.Code)
		}
	}
	if err := os.Symlink(file, filepath.Join(dir, "link")); err == nil {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/dist/link", nil))
		if w.Code != 404 {
			t.Fatal("symlink exposed")
		}
	}
	r := httptest.NewRequest("GET", "/pkg", nil)
	r.Header.Set("Range", "bytes=1-3")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "bcd" {
		t.Fatal("range failed")
	}
}

func TestShareBasicAuthFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "auth")
	if err := os.WriteFile(file, []byte("team:file:password\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	auth, err := loadShareBasicAuth("", "", file)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/dist", nil)
	r.SetBasicAuth("team", "file:password")
	if !auth.accepts(r) {
		t.Fatal("credentials from file were rejected")
	}
	if _, err := loadShareBasicAuth("team", "other", file); err == nil {
		t.Fatal("mixed credential sources accepted")
	}
	if _, err := loadShareBasicAuth("team", "", ""); err == nil {
		t.Fatal("missing password accepted")
	}
}

func TestTemporaryAgentCommand(t *testing.T) {
	binary, cleanup := buildWebhook(t)
	t.Cleanup(cleanup)
	dir := t.TempDir()
	cfg := `mode: hub
hub:
  transport_prefix: /agent/v1
  share_prefix: /downloads
  share_public_url: %q
  nodes:
    - id: temp
      token_env: NETWORK_CLI_TEST_TOKEN
      services: [files]
      allow_share_links: true
    - id: ops
      token_env: NETWORK_CLI_TEST_OPS_TOKEN
      allow: [temp/files]
`
	path := filepath.Join(dir, "hub.yaml")
	shared := filepath.Join(dir, "shared")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(shared, "file.txt")
	if err := os.WriteFile(file, []byte("shared"), 0600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(shared, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "space & file.txt"), []byte("nested"), 0600); err != nil {
		t.Fatal(err)
	}
	host, port := serverAddress(t)
	origin := net.JoinHostPort(host, port)
	env := append(webhookEnv(),
		"NETWORK_CLI_TEST_TOKEN="+strings.Repeat("s", 64),
		"NETWORK_CLI_TEST_OPS_TOKEN="+strings.Repeat("o", 64))
	start := func(output io.Writer, args ...string) *exec.Cmd {
		cmd := exec.Command(binary, args...)
		cmd.Env = env
		cmd.Stdout = output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { killAndWait(cmd) })
		return cmd
	}
	upstream, err := url.Parse("http://" + origin)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/wh1/", http.StripPrefix("/wh1", httputil.NewSingleHostReverseProxy(upstream)))
	downloadUpstream := *upstream
	downloadUpstream.Path = "/downloads"
	mux.Handle("/s/", http.StripPrefix("/s", httputil.NewSingleHostReverseProxy(&downloadUpstream)))
	proxy := httptest.NewServer(mux)
	t.Cleanup(proxy.Close)
	if err := os.WriteFile(path, []byte(fmt.Sprintf(cfg, proxy.URL+"/s")), 0600); err != nil {
		t.Fatal(err)
	}
	start(nil, "-network-config", path, "-ip", host, "-port", port)
	waitForServerReady(t, origin, http.DefaultClient)
	serverURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/wh1/"
	output, err := os.CreateTemp(dir, "share-links-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	sharingAgent := start(output, "agent", "--server", serverURL, "--id", "temp",
		"--transport-prefix", "/agent/v1",
		"--token-env", "NETWORK_CLI_TEST_TOKEN", "--share-link", "--share", "pkg="+file,
		"--share", "dist="+shared, "--duration", "30s")
	localHost, localPort := serverAddress(t)
	entry := net.JoinHostPort(localHost, localPort)
	forwardingAgent := start(nil, "agent", "--server", serverURL, "--id", "ops",
		"--transport-prefix", "/agent/v1",
		"--token-env", "NETWORK_CLI_TEST_OPS_TOKEN", "--forward", entry+"=temp/files", "--duration", "30s")
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := (&http.Client{Timeout: time.Second}).Get("http://" + entry + "/pkg")
		if err == nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode == 200 && string(body) == "shared" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary share unavailable: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	killAndWait(forwardingAgent)

	links := readAgentShareLinks(t, output.Name(), 2)
	if !strings.HasPrefix(links[0], proxy.URL+"/s/") {
		t.Fatal("download URL did not use the separate public path")
	}
	client := &http.Client{Timeout: time.Second}
	var authPassword string
	download := func(method, link, byteRange string) (int, http.Header, string) {
		t.Helper()
		request, err := http.NewRequest(method, link, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Range", byteRange)
		if authPassword != "" {
			request.SetBasicAuth("team", authPassword)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("download request failed")
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, response.Header, string(body)
	}
	status, headers, body := download("GET", links[0], "bytes=1-3")
	if status != 206 || body != "har" || headers.Get("Content-Disposition") != "attachment; filename=file.txt" || headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("invalid range download: %d %v %q", status, headers, body)
	}
	if status, _, body := download("HEAD", links[0], ""); status != 200 || body != "" {
		t.Fatal("download HEAD failed")
	}
	if status, _, body := download("GET", links[1]+"/file.txt", ""); status != 200 || body != "shared" {
		t.Fatal("directory download failed")
	}
	if status, _, _ := download("GET", strings.TrimSuffix(links[0], "/pkg")+"/dist/file.txt", ""); status != 404 {
		t.Fatal("single-file link authorized another share")
	}
	if status, _, _ := download("POST", links[0], ""); status != 405 {
		t.Fatal("download route allowed POST")
	}

	connect := func(node, tokenEnv string) *exec.Cmd {
		cmd := exec.Command(binary, "agent", "--server", serverURL, "--id", node, "--token-env",
			tokenEnv, "--transport-prefix", "/agent/v1", "--connect", "temp/files", "--duration", "3s")
		cmd.Env = env
		cmd.Stdin = strings.NewReader("GET /pkg HTTP/1.1\r\nHost: local\r\nConnection: close\r\n\r\n")
		return cmd
	}
	raw, err := connect("ops", "NETWORK_CLI_TEST_OPS_TOKEN").Output()
	if err != nil {
		t.Fatalf("authorized direct connection failed: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, err := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || err != nil || string(bodyBytes) != "shared" {
		t.Fatal("invalid direct connection response")
	}
	if raw, err := connect("temp", "NETWORK_CLI_TEST_TOKEN").Output(); err == nil || len(raw) != 0 {
		t.Fatal("unauthorized direct connection succeeded or wrote to stdout")
	}
	killAndWait(sharingAgent)
	deadline = time.Now().Add(3 * time.Second)
	for {
		if status, _, _ := download("GET", links[0], ""); status == 404 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("download link remained active after Agent stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}

	basicOutput, err := os.CreateTemp(dir, "basic-links-")
	if err != nil {
		t.Fatal(err)
	}
	defer basicOutput.Close()
	start(basicOutput, "agent", "--server", serverURL, "--id", "temp", "--transport-prefix", "/agent/v1",
		"--token-env", "NETWORK_CLI_TEST_TOKEN", "--share-link", "--share-index", "--share", "dist="+shared,
		"--share-basic-user", "team", "--share-basic-password", "test-password", "--duration", "30s")
	basicLink := readAgentShareLinks(t, basicOutput.Name(), 1)[0]
	if status, headers, _ := download("GET", basicLink, ""); status != 401 || headers.Get("WWW-Authenticate") != network.ShareBasicChallenge {
		t.Fatal("share did not forward the agent's Basic Auth challenge")
	}
	authPassword = "wrong"
	if status, _, _ := download("GET", basicLink, ""); status != 401 {
		t.Fatal("share accepted invalid credentials")
	}
	authPassword = "test-password"
	if status, headers, body := download("GET", basicLink, ""); status != 200 ||
		headers.Get("Content-Disposition") != "" || !strings.Contains(body, `href="./dist/nested/"`) ||
		!strings.Contains(headers.Get("Content-Security-Policy"), "sandbox allow-downloads") {
		t.Fatalf("unexpected browsable index: %d %v %q", status, headers, body)
	}
	if status, _, body := download("GET", basicLink+"/nested/", ""); status != 200 ||
		!strings.Contains(body, `href="./space%20&amp;%20file.txt"`) {
		t.Fatalf("unexpected nested directory index: %d %q", status, body)
	}
	if status, _, body := download("GET", basicLink+"/nested/space%20%26%20file.txt", ""); status != 200 || body != "nested" {
		t.Fatal("file listed in directory could not be downloaded")
	}
	if status, _, _ := download("GET", links[1], ""); status != 404 {
		t.Fatal("a new sharing session restored an obsolete link")
	}
}

func readAgentShareLinks(t *testing.T, file string, count int) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(content), "\n")
		if len(lines) >= count+2 {
			return lines[1 : count+1]
		}
		if time.Now().After(deadline) {
			t.Fatal("temporary agent did not print download links")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAsyncMultipartSurvivesResponseCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell command")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("pkg", "app.war")
	io.WriteString(part, "package")
	form.Close()
	raw := httptest.NewRequest("POST", "/hooks/deploy", &body)
	raw.Header.Set("Content-Type", form.FormDataContentType())
	if err := raw.ParseMultipartForm(1); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "received")
	h := &hook.Hook{ID: "async-file-test", ExecuteCommand: "/bin/sh", KeepFileEnvironment: true, PassArgumentsToCommand: []hook.Argument{{Source: "string", Name: "-c"}, {Source: "string", Name: `sleep 0.1; cp "$HOOK_FILE_PKG" "$1"`}, {Source: "string", Name: "sh"}, {Source: "string", Name: target}}}
	if err := handleHookAsync(h, &hook.Request{ID: "test", RawRequest: raw}); err != nil {
		t.Fatal(err)
	}
	// net/http performs this cleanup after the HTTP handler returns.
	if raw.MultipartForm != nil {
		raw.MultipartForm.RemoveAll()
		t.Fatal("multipart ownership was not detached")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(target)
		if err == nil && string(data) == "package" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async command lost its upload")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTemporaryIdleAgentCLI(t *testing.T) {
	binary, cleanup := buildWebhook(t)
	t.Cleanup(cleanup)
	token := strings.Repeat("t", 64)
	cfg, err := network.ParseConfig([]byte("mode: hub\nhub:\n  nodes:\n    - id: idle\n      token: "+token+"\n"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub := network.NewHub(cfg.Hub)
	defer hub.Close()
	server := httptest.NewServer(hub)
	defer server.Close()
	cmd := exec.Command(binary, "agent", "--server", "ws"+strings.TrimPrefix(server.URL, "http"), "--id", "idle", "--token", token, "--duration", "2s")
	cmd.Env = webhookEnv()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer killAndWait(cmd)
	deadline := time.Now().Add(time.Second)
	for {
		node := hub.Status().Nodes[0]
		if node.Status == "online" {
			if node.Version != version || !node.Temporary || node.ExpiresAt == nil || len(node.Services) != 0 {
				t.Fatalf("invalid CLI metadata: %+v", node)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle CLI did not join hub")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("temporary CLI did not exit cleanly: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for hub.Status().Nodes[0].Status != "offline" {
		if time.Now().After(deadline) {
			t.Fatal("expired CLI still online")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
