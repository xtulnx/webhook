//go:build !windows

package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLongRuntimeDirectoryUsesPrivateSocketFallback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("long-directory-", 8))
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	id := strings.Repeat("a", 32)
	socket := runtimeSocketPath(dir, id)
	if len(socket) > 90 || filepath.Dir(socket) == dir {
		t.Fatal("socket path not shortened", socket)
	}
	ln, e := listenRuntimeControl(dir, id)
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	defer removeRuntimeEndpoint(dir, id)
	if e = secureRuntimeDirectory(filepath.Dir(socket), false); e != nil {
		t.Fatal("fallback is not private", e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, e := dialRuntimeControl(ctx, dir, id)
	if e != nil {
		t.Fatal(e)
	}
	conn.Close()
	ln.Close()
	removeRuntimeEndpoint(dir, id)
	if _, e = os.Stat(filepath.Dir(socket)); !os.IsNotExist(e) {
		t.Fatal("fallback directory not cleaned up")
	}
}
