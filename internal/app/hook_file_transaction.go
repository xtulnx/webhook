package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/xtulnx/webhook/internal/hook"
)

// Directory locks remain on disk: unlinking an unlocked lock file races with
// another process opening it. All cooperating writers lock in canonical order.
func lockHookDirectories(paths []string) (func(), error) {
	dirs := map[string]bool{}
	for _, p := range paths {
		d, e := filepath.EvalSymlinks(filepath.Dir(p))
		if e != nil {
			return nil, e
		}
		d, e = filepath.Abs(d)
		if e != nil {
			return nil, e
		}
		dirs[d] = true
	}
	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		ordered = append(ordered, d)
	}
	sort.Strings(ordered)
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, d := range ordered {
		unlock, e := lockHookDirectory(d)
		if e != nil {
			release()
			return nil, e
		}
		releases = append(releases, unlock)
	}
	return release, nil
}

func openHookLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, ".webhook-hooks.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		if err = prepareHookLockFile(f, dir); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	if !os.IsExist(err) {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("Hook lock is not a regular file")
	}
	f, err = os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		f.Close()
		return nil, errors.New("Hook lock changed while opening")
	}
	return f, nil
}

type hookDiskSnapshot struct {
	path  string
	info  os.FileInfo
	data  []byte
	hooks hook.Hooks
}

func readHookSnapshot(path string) (hookDiskSnapshot, error) {
	var s hookDiskSnapshot
	s.path = path
	info, err := os.Lstat(path)
	if err != nil {
		return s, err
	}
	if !info.Mode().IsRegular() {
		return s, fmt.Errorf("Hook write source must be a regular file, not a symlink: %s", path)
	}
	s.info = info
	s.data, err = os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err = s.hooks.LoadFromBytesStrict(s.data); err != nil {
		return s, fmt.Errorf("invalid or unknown Hook fields in %s", path)
	}
	return s, nil
}
func (s hookDiskSnapshot) unchanged() error {
	info, err := os.Lstat(s.path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(s.info, info) {
		return errors.New("Hook source replaced concurrently")
	}
	current, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, s.data) {
		return errors.New("Hook source edited concurrently")
	}
	return nil
}

func writeHookReplacement(path string, data []byte, original os.FileInfo) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".webhook-write-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	defer tmp.Close()
	if original != nil {
		if err = preserveHookFileMetadata(tmp, original, path); err != nil {
			return err
		}
	} else if err = tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return replaceHookFile(name, path, original != nil)
}

// commitHookSnapshot is called only while all participating directory locks are held.
// External editors do not cooperate; the final comparison detects known changes
// but does not claim filesystem compare-and-swap semantics.
func commitHookSnapshot(all []hookDiskSnapshot, target string, next hook.Hooks) (string, error) {
	var source *hookDiskSnapshot
	for i := range all {
		if all[i].path == target {
			source = &all[i]
		}
		if err := all[i].unchanged(); err != nil {
			return "", err
		}
	}
	if source == nil {
		return "", errUnknownHookFile
	}
	data, err := marshalHooksFile(target, next)
	if err != nil {
		return "", err
	}
	backup, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".backup-*")
	if err != nil {
		return "", err
	}
	name := backup.Name()
	ok := false
	defer func() {
		backup.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	if err = preserveHookFileMetadata(backup, source.info, target); err != nil {
		return "", err
	}
	if _, err = backup.Write(source.data); err != nil {
		return "", err
	}
	if err = backup.Sync(); err != nil {
		return "", err
	}
	if err = backup.Close(); err != nil {
		return "", err
	}
	for _, s := range all {
		if err = s.unchanged(); err != nil {
			return "", err
		}
	}
	if err = writeHookReplacement(target, data, source.info); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}
