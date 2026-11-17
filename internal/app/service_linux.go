//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	systemdbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

type nativeSystemd struct {
	conn    *systemdbus.Conn
	manager godbus.BusObject
}

func (n nativeSystemd) State(ctx context.Context) (map[string]interface{}, error) {
	var path godbus.ObjectPath
	err := n.manager.CallWithContext(ctx, "org.freedesktop.systemd1.Manager.LoadUnit", 0, managedServiceName).Store(&path)
	if err != nil {
		var value godbus.Error
		var ptr *godbus.Error
		name := ""
		if errors.As(err, &value) {
			name = value.Name
		} else if errors.As(err, &ptr) {
			name = ptr.Name
		}
		if name == "org.freedesktop.systemd1.NoSuchUnit" {
			return map[string]interface{}{"LoadState": "not-found"}, nil
		}
		return nil, err
	}
	return n.conn.GetUnitPropertiesContext(ctx, managedServiceName)
}
func (n nativeSystemd) Reload(ctx context.Context) error { return n.conn.ReloadContext(ctx) }
func (n nativeSystemd) SetEnabled(ctx context.Context, enable bool) error {
	if enable {
		_, _, e := n.conn.EnableUnitFilesContext(ctx, []string{managedServiceName}, false, false)
		return e
	}
	_, e := n.conn.DisableUnitFilesContext(ctx, []string{managedServiceName}, false)
	return e
}
func (n nativeSystemd) Job(ctx context.Context, action string) error {
	done := make(chan string, 1)
	var e error
	switch action {
	case "start":
		_, e = n.conn.StartUnitContext(ctx, managedServiceName, "fail", done)
	case "stop":
		_, e = n.conn.StopUnitContext(ctx, managedServiceName, "fail", done)
	case "restart":
		_, e = n.conn.RestartUnitContext(ctx, managedServiceName, "fail", done)
	}
	if e != nil {
		return e
	}
	select {
	case result := <-done:
		if result != "done" {
			return fmt.Errorf("systemd job: %s", result)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("服务操作未确认，系统可能仍在执行：%w", ctx.Err())
	}
}

func servicePlatformOperation(ctx context.Context, o serviceOptions) (interface{}, error) {
	if o.Action == "install" && o.DryRun {
		unit, e := renderManagedUnit(o.Spec)
		if e != nil {
			return nil, e
		}
		return map[string]interface{}{"dryRun": true, "unitPath": managedUnitPath, "unit": string(unit), "spec": o.Spec, "started": false, "enabled": false}, nil
	}
	if o.Action == "install" {
		if e := checkServiceUserAccess(o.Spec); e != nil {
			return nil, e
		}
	}
	if o.Action != "status" && os.Geteuid() != 0 {
		return nil, errors.New("系统级服务管理需要已有 root 权限，不自动提权")
	}
	var manager godbus.BusObject
	conn, e := systemdbus.NewConnection(func() (*godbus.Conn, error) {
		var bus *godbus.Conn
		var err error
		if os.Geteuid() == 0 {
			bus, err = godbus.Dial("unix:path=/run/systemd/private", godbus.WithContext(ctx))
		} else {
			bus, err = godbus.SystemBusPrivate(godbus.WithContext(ctx))
		}
		if err != nil {
			return nil, err
		}
		if err = bus.Auth([]godbus.Auth{godbus.AuthExternal(strconv.Itoa(os.Geteuid()))}); err != nil {
			bus.Close()
			return nil, err
		}
		if os.Geteuid() != 0 {
			if err = bus.Hello(); err != nil {
				bus.Close()
				return nil, err
			}
		}
		manager = bus.Object("org.freedesktop.systemd1", godbus.ObjectPath("/org/freedesktop/systemd1"))
		return bus, nil
	})
	if e != nil {
		return nil, fmt.Errorf("无法连接本机 systemd：%w", e)
	}
	defer conn.Close()
	return manageSystemd(ctx, nativeSystemd{conn, manager}, managedUnitPath, o, systemdFiles{readManagedUnit, prepareServiceRuntime})
}

func readManagedUnit(path string) (serviceInstallSpec, []byte, error) {
	var s serviceInstallSpec
	info, e := os.Lstat(path)
	if e != nil {
		return s, nil, e
	}
	if !info.Mode().IsRegular() {
		return s, nil, errors.New("服务单元必须是普通文件，不能是符号链接")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return s, nil, errors.New("服务单元必须由 root 拥有且不能被组或其他用户写入")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return s, nil, e
	}
	s, e = parseManagedUnit(raw)
	return s, raw, e
}
func installedServiceRuntime(config string) (string, error) {
	info, e := os.Lstat(managedUnitPath)
	if os.IsNotExist(e) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	raw, e := os.ReadFile(managedUnitPath)
	if e != nil {
		return "", e
	}
	if !strings.HasPrefix(string(raw), serviceMarker) {
		return "", nil
	}
	s, _, e := readManagedUnit(managedUnitPath)
	if e != nil {
		return "", e
	}
	if s.Config != config {
		return "", nil
	}
	return s.RuntimeDirectory, nil
}

func prepareServiceRuntime(s serviceInstallSpec) (func(), error) {
	if s.RuntimeDirectory == "/run/webhook" {
		return func() {}, nil
	}
	uid := s.UID
	var e error
	created := false
	if e = os.Mkdir(s.RuntimeDirectory, 0700); e == nil {
		created = true
		if e = os.Chown(s.RuntimeDirectory, uid, s.GID); e != nil {
			os.Remove(s.RuntimeDirectory)
			return nil, e
		}
	} else if !os.IsExist(e) {
		return nil, e
	}
	cleanup := func() {
		if created {
			_ = os.Remove(s.RuntimeDirectory)
		}
	}
	info, e := os.Lstat(s.RuntimeDirectory)
	if e != nil {
		cleanup()
		return nil, e
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || int(stat.Uid) != s.UID {
		cleanup()
		return nil, errors.New("runtime-dir 必须是服务用户独占的私有目录，不自动更改已有目录权限")
	}
	return cleanup, nil
}

// Check ordinary POSIX permissions without executing any user command or
// changing the process credentials. Extra ACL grants may require manual review.
func checkServiceUserAccess(s serviceInstallSpec) error {
	u, e := user.Lookup(s.User)
	if e != nil {
		return e
	}
	ids, e := u.GroupIds()
	if e != nil {
		return e
	}
	groups := map[uint32]bool{}
	for _, id := range ids {
		n, e := strconv.ParseUint(id, 10, 32)
		if e != nil {
			return e
		}
		groups[uint32(n)] = true
	}
	check := func(path string, required uint32) error {
		info, e := os.Stat(path)
		if e != nil {
			return e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("无法检查服务账号权限")
		}
		if s.UID == 0 {
			return nil
		}
		bits := uint32(info.Mode().Perm())
		if uint32(s.UID) == st.Uid {
			bits >>= 6
		} else if groups[st.Gid] {
			bits >>= 3
		}
		if bits&required != required {
			return fmt.Errorf("服务账号 %s 缺少路径 %s 的必要权限；不会自动放宽业务权限", s.User, path)
		}
		return nil
	}
	paths := map[string]uint32{s.Config: 4, s.Executable: 1, s.Directory: 1}
	files, e := readLocalHooks(localHookOptions{Config: s.Config, Directory: s.Directory})
	if e != nil {
		return e
	}
	for _, f := range files {
		paths[f.Path] = 4
	}
	for path, mode := range paths {
		if e = check(path, mode); e != nil {
			return e
		}
		for d := filepath.Dir(path); ; d = filepath.Dir(d) {
			if e = check(d, 1); e != nil {
				return e
			}
			if d == filepath.Dir(d) {
				break
			}
		}
	}
	return nil
}
