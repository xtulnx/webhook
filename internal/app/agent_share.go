package app

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xtulnx/webhook/internal/network"
)

type shareBasicAuth struct {
	user     [sha256.Size]byte
	password [sha256.Size]byte
}

func loadShareBasicAuth(user, password, file string) (*shareBasicAuth, error) {
	if file != "" {
		if user != "" || password != "" {
			return nil, errors.New("choose either a share Basic Auth file or a username and password")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, errors.New("cannot read share Basic Auth file")
		}
		user, password, _ = strings.Cut(strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"), ":")
	}
	if file == "" && user == "" && password == "" {
		return nil, nil
	}
	if user == "" || password == "" || strings.ContainsAny(user, ":\r\n\x00") || strings.ContainsAny(password, "\r\n\x00") {
		return nil, errors.New("share Basic Auth requires a non-empty username and password without line breaks or NUL; username must not contain a colon")
	}
	return &shareBasicAuth{sha256.Sum256([]byte(user)), sha256.Sum256([]byte(password))}, nil
}

func (a *shareBasicAuth) accepts(r *http.Request) bool {
	user, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	userHash, passwordHash := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(a.user[:], userHash[:])&subtle.ConstantTimeCompare(a.password[:], passwordHash[:]) == 1
}

type shareRoot struct {
	path string
	dir  bool
}

func newShareHandler(values []string, index bool, auth *shareBasicAuth) (http.Handler, error) {
	roots := make(map[string]shareRoot, len(values))
	for _, value := range values {
		name, path, ok := strings.Cut(value, "=")
		if !ok || !network.ValidShareName(name) {
			return nil, errors.New("share requires a simple URL-name=path")
		}
		if _, exists := roots[name]; exists {
			return nil, errors.New("duplicate share name")
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil, errors.New("share must be a regular file or directory")
		}
		roots[name] = shareRoot{path, info.IsDir()}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if auth != nil && !auth.accepts(r) {
			w.Header().Set("WWW-Authenticate", network.ShareBasicChallenge)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		file, info := openShareFile(r.URL.Path, roots)
		if file == nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		if info.IsDir() {
			if index {
				serveShareIndex(w, r, file)
				return
			}
			http.NotFound(w, r)
			return
		}
		shareName, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		indexedDirectory := index && roots[shareName].dir
		if indexedDirectory || r.Header.Get(network.ShareDownloadHeader) == "1" {
			disposition := "attachment"
			if previewType := network.SharePreviewType(info.Name()); indexedDirectory && previewType != "" {
				disposition = "inline"
				w.Header().Set("Content-Type", previewType)
				w.Header().Set("X-Content-Type-Options", "nosniff")
			}
			w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": info.Name()}))
		}
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	}), nil
}

func validShareEntry(name string) bool {
	return strings.TrimRight(name, ". ") != "" && !strings.ContainsAny(name, "\\\x00:")
}

func openShareFile(requestPath string, roots map[string]shareRoot) (*os.File, os.FileInfo) {
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(requestPath, "/"), "/"), "/")
	root, ok := roots[parts[0]]
	if !ok || !root.dir && len(parts) != 1 {
		return nil, nil
	}
	target := root.path
	for _, part := range parts[1:] {
		if !validShareEntry(part) {
			return nil, nil
		}
		target = filepath.Join(target, part)
		info, err := os.Lstat(target)
		if err != nil || !(info.IsDir() || info.Mode().IsRegular()) {
			return nil, nil
		}
	}
	info, err := os.Lstat(target)
	if err != nil || !(info.IsDir() || info.Mode().IsRegular()) ||
		(strings.HasSuffix(requestPath, "/") && !info.IsDir()) {
		return nil, nil
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, nil
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, nil
	}
	return file, opened
}

var shareIndexPage = template.Must(template.New("share-index").Parse(
	`<!doctype html><html lang="en"><meta charset="utf-8"><title>Shared files</title>
<h1>Shared files</h1><ul>{{range .}}<li><a href="{{.Href}}">{{.Name}}</a></li>{{else}}<li>No files</li>{{end}}</ul></html>`))

func serveShareIndex(w http.ResponseWriter, r *http.Request, dir *os.File) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", network.ShareIndexCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set(network.ShareIndexHeader, "1")
	if r.Method == http.MethodHead {
		return
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		http.Error(w, "cannot list shared directory", http.StatusInternalServerError)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	type link struct{ Name, Href string }
	var links []link
	base := "./"
	if !strings.HasSuffix(r.URL.Path, "/") {
		base += path.Base(r.URL.Path) + "/"
	}
	for _, entry := range entries {
		name := entry.Name()
		if !validShareEntry(name) || !(entry.IsDir() || entry.Type().IsRegular()) {
			continue
		}
		if entry.IsDir() {
			name += "/"
		}
		u := url.URL{Path: base + name}
		links = append(links, link{Name: name, Href: u.EscapedPath()})
	}
	_ = shareIndexPage.Execute(w, links)
}
