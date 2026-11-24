package app

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const shareUploadTempPrefix = ".webhook-share-upload-"

func serveShareUpload(w http.ResponseWriter, r *http.Request, roots map[string]shareRoot, commit *sync.Mutex) {
	shareName, name, hasSuffix := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	root, ok := roots[shareName]
	if !ok || !root.dir && hasSuffix {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Content-Range") != "" {
		http.Error(w, "upload must contain a complete file", http.StatusBadRequest)
		return
	}
	dir := root.path
	if root.dir {
		if !validShareEntry(name) || !filepath.IsLocal(name) || strings.Contains(name, "/") || strings.TrimRight(name, ". ") != name {
			http.Error(w, "upload URL must end with a single file name", http.StatusBadRequest)
			return
		}
	} else {
		dir, name = filepath.Dir(root.path), filepath.Base(root.path)
	}
	status := saveShareUpload(r, dir, name, root, commit)
	if status >= http.StatusBadRequest {
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.WriteHeader(status)
}

func saveShareUpload(r *http.Request, dir, name string, root shareRoot, commit *sync.Mutex) int {
	// Check the resolved directory again before both staging and publishing.
	dirUnchanged := func() bool {
		info, err := os.Lstat(dir)
		return err == nil && info.IsDir() && os.SameFile(root.uploadDirInfo, info)
	}
	if !dirUnchanged() {
		return http.StatusNotFound
	}
	tmp, err := os.CreateTemp(dir, shareUploadTempPrefix+"*")
	if err != nil {
		return http.StatusInternalServerError
	}
	published := false
	defer func() {
		if !published {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	n, err := io.Copy(tmp, r.Body)
	if err != nil {
		var diskErr *os.PathError
		if errors.As(err, &diskErr) && diskErr.Path == tmp.Name() {
			return http.StatusInternalServerError
		}
		return http.StatusBadRequest
	}
	if (r.ContentLength >= 0 && n != r.ContentLength) || r.Context().Err() != nil {
		return http.StatusBadRequest
	}
	// Only the final check and replacement need serialization between uploads.
	commit.Lock()
	defer commit.Unlock()
	if !dirUnchanged() {
		return http.StatusNotFound
	}
	target := filepath.Join(dir, name)
	info, err := os.Lstat(target)
	exists := err == nil
	switch {
	case exists && !info.Mode().IsRegular():
		return http.StatusConflict
	case err != nil && !os.IsNotExist(err):
		return http.StatusInternalServerError
	case !exists && !root.dir:
		return http.StatusNotFound
	case exists:
		if err := preserveHookFileMetadata(tmp, info, target); err != nil {
			return http.StatusInternalServerError
		}
	}
	if err := tmp.Sync(); err != nil {
		return http.StatusInternalServerError
	}
	if err := tmp.Close(); err != nil {
		return http.StatusInternalServerError
	}
	if r.Context().Err() != nil {
		return http.StatusBadRequest
	}
	if err := replaceHookFile(tmp.Name(), target, exists); err != nil {
		return http.StatusInternalServerError
	}
	published = true
	if exists {
		return http.StatusNoContent
	}
	return http.StatusCreated
}
