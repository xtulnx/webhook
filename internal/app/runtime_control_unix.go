//go:build !windows

package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func secureRuntimeDirectory(dir string, create bool) error {
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("runtime-dir 必须是当前用户独占的 0700 目录")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() && os.Geteuid() != 0 {
		return errors.New("runtime-dir 所有者不匹配")
	}
	return nil
}
func runtimeSocketPath(dir, id string) string {
	path := filepath.Join(dir, id[:12]+".sock")
	// sockaddr_un has a small OS-dependent limit (104 bytes on macOS).
	// A deterministic, private directory also works across different users' TMPDIRs.
	if len(path) > 90 {
		sum := sha256.Sum256([]byte(filepath.Clean(dir) + "\x00" + id))
		return filepath.Join("/tmp", fmt.Sprintf("wh-%x", sum[:16]), "control.sock")
	}
	return path
}
func listenRuntimeControl(dir, id string) (net.Listener, error) {
	path := runtimeSocketPath(dir, id)
	fallback := filepath.Dir(path) != filepath.Clean(dir)
	if fallback {
		if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil && fallback {
		_ = os.Remove(filepath.Dir(path))
	}
	return ln, err
}
func dialRuntimeControl(ctx context.Context, dir, id string) (net.Conn, error) {
	path := runtimeSocketPath(dir, id)
	if filepath.Dir(path) != filepath.Clean(dir) {
		if err := secureRuntimeDirectory(filepath.Dir(path), false); err != nil {
			return nil, err
		}
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
func removeRuntimeEndpoint(dir, id string) {
	path := runtimeSocketPath(dir, id)
	_ = os.Remove(path)
	if filepath.Dir(path) != filepath.Clean(dir) {
		_ = os.Remove(filepath.Dir(path))
	}
}
func runtimeEndpointAbsent(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}
