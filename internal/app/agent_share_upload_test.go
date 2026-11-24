package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShareUpload(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	if err := os.MkdirAll(filepath.Join(shared, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "original.txt")
	if err := os.WriteFile(file, []byte("original"), 0640); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(shared, "same-inode.txt")
	linkErr := os.Link(file, alias)
	handler, err := newShareHandler([]string{"file=" + file, "dir=" + shared}, true, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	upload := func(path, contents string, length int64, contentRange string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodPut, path, strings.NewReader(contents))
		if length >= 0 {
			r.ContentLength = length
		}
		if contentRange != "" {
			r.Header.Set("Content-Range", contentRange)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/file/other.txt", http.StatusNotFound},
		{"/dir", http.StatusBadRequest},
		{"/dir/nested/other.txt", http.StatusBadRequest},
		{"/dir/%2e%2e%2foriginal.txt", http.StatusBadRequest},
		{"/dir/other%5cfile.txt", http.StatusBadRequest},
		{"/dir/original.txt:stream", http.StatusBadRequest},
		{"/dir/nested", http.StatusConflict},
		{"/dir/" + shareUploadTempPrefix + "test", http.StatusBadRequest},
	} {
		if got := upload(tc.path, "denied", -1, ""); got != tc.want {
			t.Errorf("PUT %s: got %d, want %d", tc.path, got, tc.want)
		}
	}
	if err := os.Symlink(file, filepath.Join(shared, "link.txt")); err == nil {
		if got := upload("/dir/link.txt", "denied", -1, ""); got != http.StatusConflict {
			t.Errorf("PUT to a symlink returned %d", got)
		}
	}
	if got := upload("/file", "truncated", 100, ""); got != http.StatusBadRequest {
		t.Errorf("PUT with an incomplete body returned %d", got)
	}
	if got := upload("/file", "partial", -1, "bytes 0-6/10"); got != http.StatusBadRequest {
		t.Errorf("PUT with Content-Range returned %d", got)
	}
	assertContents := func(name, want string) {
		t.Helper()
		data, err := os.ReadFile(name)
		if err != nil || string(data) != want {
			t.Fatalf("%s contains %q, want %q, error %v", name, data, want, err)
		}
	}
	assertContents(file, "original")
	if linkErr == nil {
		if got := upload("/dir/same-inode.txt", "inside", -1, ""); got != http.StatusNoContent {
			t.Fatalf("PUT to a hard-linked name returned %d", got)
		}
		assertContents(file, "original")
		assertContents(alias, "inside")
	}
	if got := upload("/file", "", -1, ""); got != http.StatusNoContent {
		t.Fatalf("PUT of an empty file returned %d", got)
	}
	assertContents(file, "")
	if got := upload("/dir/upload.txt", "first", -1, ""); got != http.StatusCreated {
		t.Fatalf("new directory upload returned %d", got)
	}
	if got := upload("/dir/upload.txt", "second", -1, ""); got != http.StatusNoContent {
		t.Fatalf("directory overwrite returned %d", got)
	}
	assertContents(filepath.Join(shared, "upload.txt"), "second")
	for _, path := range []string{dir, shared} {
		if files, err := filepath.Glob(filepath.Join(path, shareUploadTempPrefix+"*")); err != nil || len(files) != 0 {
			t.Errorf("temporary upload files in %s: %v, %v", path, files, err)
		}
	}
}
