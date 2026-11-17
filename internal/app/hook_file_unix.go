//go:build !windows

package app

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func lockHookDirectory(dir string) (func(), error) {
	f, err := openHookLock(dir)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("Hook configuration is locked by another writer: %w", err)
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}
func preserveHookFileMetadata(f *os.File, original os.FileInfo, _ string) error {
	stat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot preserve Hook ownership")
	}
	if err := f.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
		return err
	}
	return f.Chmod(original.Mode().Perm())
}
func replaceHookFile(from, to string, _ bool) error { return os.Rename(from, to) }

// A privileged CLI must not leave a root-only lock in a service-owned directory.
func prepareHookLockFile(f *os.File, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine Hook lock ownership")
	}
	uid := os.Geteuid()
	if uid == 0 {
		uid = int(stat.Uid)
	}
	if err = f.Chown(uid, int(stat.Gid)); err != nil {
		return err
	}
	mode := os.FileMode(0600)
	if info.Mode().Perm()&0020 != 0 {
		mode = 0660
	}
	return f.Chmod(mode)
}
