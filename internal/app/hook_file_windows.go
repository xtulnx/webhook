//go:build windows

package app

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func lockHookDirectory(dir string) (func(), error) {
	f, err := openHookLock(dir)
	if err != nil {
		return nil, err
	}
	overlap := &windows.Overlapped{}
	if err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlap); err != nil {
		f.Close()
		return nil, fmt.Errorf("Hook configuration is locked by another writer: %w", err)
	}
	return func() { _ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlap); f.Close() }, nil
}
func preserveHookFileMetadata(f *os.File, _ os.FileInfo, path string) error {
	const flags = windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, flags)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	group, _, err := sd.Group()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(f.Name(), windows.SE_FILE_OBJECT, flags|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, group, dacl, nil)
}
func replaceHookFile(from, to string, exists bool) error {
	if !exists {
		return os.Rename(from, to)
	}
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")
	result, _, callErr := proc.Call(uintptr(unsafe.Pointer(target)), uintptr(unsafe.Pointer(source)), 0, 0, 0, 0)
	if result == 0 {
		return callErr
	}
	return nil
}

// Lock files contain no configuration data and inherit the source directory ACL.
func prepareHookLockFile(_ *os.File, _ string) error { return nil }
