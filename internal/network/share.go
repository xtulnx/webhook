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
	"path"
	"strings"
	"time"
)

const SharePath = "/__webhook_network/v1/share/"
const ShareDownloadHeader = "X-Webhook-Share-Download"
const ShareIndexHeader = "X-Webhook-Share-Index"
const ShareIndexCSP = "sandbox allow-downloads; default-src 'none'; base-uri 'none'; form-action 'none'"
const ShareBasicChallenge = `Basic realm="webhook share", charset="UTF-8"`

// SharePreviewType returns the media type for files eligible for browser preview.
func SharePreviewType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".txt", ".text", ".md", ".markdown", ".yaml", ".yml", ".properties", ".ini", ".conf", ".cfg", ".toml", ".log", ".csv", ".tsv", ".jsonl", ".ndjson":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".apng":
		return "image/apng"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".bmp":
		return "image/bmp"
	case ".ico":
		return "image/x-icon"
	default:
		return ""
	}
}

func shareProof(key []byte, name string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, name)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func shareLinkID(key []byte, name string, basic bool, customID *string) string {
	if customID != nil {
		return *customID
	}
	if basic {
		// The short ID identifies the temporary session. The agent checks Basic Auth.
		return base64.RawURLEncoding.EncodeToString(key[:16])
	}
	return shareProof(key, name)
}

func shareLinks(server, base, node string, key []byte, basic bool, customID *string, names []string) []string {
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
		prefix := base + "/" + node
		if id := shareLinkID(key, name, basic, customID); id != "" {
			prefix += "/" + id
		}
		links = append(links, prefix+"/"+name)
	}
	return links
}

func (h *Hub) serveShare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", ShareIndexCSP)
	node, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, h.sharePath), "/")
	var control *controlConn
	var filePath string
	if validName(node) {
		h.mu.Lock()
		control, filePath = h.shareRouteLocked(node, rest)
		h.mu.Unlock()
	}
	now := time.Now()
	canUpload := control != nil && now.Before(control.shareExpiry) && control.shareUpload
	upload := r.Method == http.MethodPut
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !upload {
		allow := "GET, HEAD"
		if canUpload {
			allow += ", PUT"
		}
		w.Header().Set("Allow", allow)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if control == nil || !now.Before(control.shareExpiry) {
		http.NotFound(w, r)
		return
	}
	if upload && !canUpload {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		Method:     r.Method,
		URL:        &url.URL{Scheme: "http", Host: "network.internal", Path: "/" + filePath},
		Header:     make(http.Header),
		Close:      true,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	if upload {
		_ = controller.SetReadDeadline(deadline)
		defer controller.SetReadDeadline(time.Time{})
		out.Body = r.Body
		out.ContentLength = r.ContentLength
		if value := r.Header.Get("Content-Range"); value != "" {
			out.Header.Set("Content-Range", value)
		}
		// Let the agent reject an unauthorized or invalid upload before the
		// transport starts writing its body into the tunnel.
		if r.ContentLength != 0 {
			out.Header.Set("Expect", "100-continue")
		}
	} else {
		for _, name := range []string{"Range", "If-Range", "If-Modified-Since", "If-Unmodified-Since"} {
			for _, value := range r.Header.Values(name) {
				out.Header.Add(name, value)
			}
		}
		out.Header.Set(ShareDownloadHeader, "1")
	}
	if control.shareBasic {
		out.Header.Set("Authorization", r.Header.Get("Authorization"))
	}
	transport := &http.Transport{
		DisableKeepAlives:     true,
		DisableCompression:    true,
		ExpectContinueTimeout: time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return h.openControlPipe(ctx, node, "files", maxHops, control)
		},
	}
	defer transport.CloseIdleConnections()
	response, err := transport.RoundTrip(out.WithContext(ctx))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded || !time.Now().Before(deadline) {
			http.Error(w, "share request timed out", http.StatusGatewayTimeout)
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
	} else if !upload && (response.StatusCode != http.StatusOK || response.Header.Get(ShareIndexHeader) != "1") {
		disposition, params, _ := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
		previewType := SharePreviewType(filePath)
		if disposition != "inline" || previewType == "" || !sharePreviewResponse(response, previewType) {
			disposition = "attachment"
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, params))
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func sharePreviewResponse(response *http.Response, previewType string) bool {
	contentType := response.Header.Get("Content-Type")
	switch response.StatusCode {
	case http.StatusOK:
		return contentType == previewType
	case http.StatusPartialContent:
		mediaType, params, _ := mime.ParseMediaType(contentType)
		return contentType == previewType || (mediaType == "multipart/byteranges" && params["boundary"] != "")
	case http.StatusNotModified:
		return true
	default:
		return false
	}
}
