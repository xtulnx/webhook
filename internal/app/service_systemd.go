package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The interface keeps lifecycle policy testable without a real system manager.
type systemdBackend interface {
	State(context.Context) (map[string]interface{}, error)
	Reload(context.Context) error
	SetEnabled(context.Context, bool) error
	Job(context.Context, string) error
}
type systemdFiles struct {
	read           func(string) (serviceInstallSpec, []byte, error)
	prepareRuntime func(serviceInstallSpec) (func(), error)
}

func manageSystemd(ctx context.Context, backend systemdBackend, path string, o serviceOptions, files systemdFiles) (interface{}, error) {
	if o.Action == "status" {
		if _, _, e := files.read(path); e != nil {
			return nil, e
		}
		state, e := backend.State(ctx)
		if e != nil {
			return nil, e
		}
		if e = checkSystemdOwnership(path, state); e != nil {
			return nil, e
		}
		return map[string]interface{}{"service": managedServiceName, "activeState": state["ActiveState"], "subState": state["SubState"], "unitFileState": state["UnitFileState"], "health": "not-checked"}, nil
	}
	// Use a persistent lock beside the unit, shared by all service commands.
	unlock, e := lockHookDirectories([]string{path})
	if e != nil {
		return nil, e
	}
	defer unlock()
	if o.Action == "install" {
		desired, e := renderManagedUnit(o.Spec)
		if e != nil {
			return nil, e
		}
		current, readErr := os.ReadFile(path)
		if readErr == nil {
			if _, _, e = files.read(path); e != nil {
				return nil, e
			}
			if string(current) != string(desired) {
				return nil, errors.New("已有受管服务配置不同，拒绝覆盖")
			}
			state, e := backend.State(ctx)
			if e != nil {
				return nil, e
			}
			if e = checkSystemdOwnership(path, state); e != nil {
				return nil, e
			}
			return map[string]interface{}{"installed": true, "changed": false, "unitPath": path}, nil
		}
		if !os.IsNotExist(readErr) {
			return nil, readErr
		}
		state, e := backend.State(ctx)
		if e == nil {
			load, _ := state["LoadState"].(string)
			if load != "not-found" {
				return nil, errors.New("systemd 已存在同名服务，拒绝接管")
			}
		} else {
			return nil, e
		}
		if _, e = os.Lstat(strings.TrimSuffix(path, ".service") + ".socket"); e == nil {
			return nil, errors.New("存在关联 socket，拒绝接管")
		}
		if entries, e := os.ReadDir(path + ".d"); e == nil && len(entries) > 0 {
			return nil, errors.New("存在 drop-in 目录，拒绝接管")
		} else if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		cleanupRuntime, e := files.prepareRuntime(o.Spec)
		if e != nil {
			return nil, e
		}
		keepRuntime := false
		defer func() {
			if !keepRuntime {
				cleanupRuntime()
			}
		}()
		f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if e != nil {
			return nil, e
		}
		written, writeErr := f.Write(desired)
		createdInfo, statErr := f.Stat()
		if writeErr == nil {
			writeErr = statErr
		}
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr == nil {
			writeErr = closeErr
		}
		if writeErr != nil {
			_ = removeManagedCreation(path, createdInfo, desired[:written])
			return nil, writeErr
		}
		if e = backend.Reload(ctx); e != nil {
			rollbackErr := removeManagedCreation(path, createdInfo, desired)
			return nil, fmt.Errorf("systemd 重载失败；新建单元回滚结果=%v，管理器状态未确认：%w", rollbackErr, e)
		}
		loaded, stateErr := backend.State(ctx)
		if stateErr == nil {
			stateErr = checkSystemdOwnership(path, loaded)
		}
		if stateErr == nil {
			active, _ := loaded["ActiveState"].(string)
			enabled, _ := loaded["UnitFileState"].(string)
			if active != "inactive" && active != "failed" {
				stateErr = errors.New("新注册的服务出现非预期运行状态")
			}
			if enabled != "disabled" {
				stateErr = errors.New("新注册服务出现非预期开机启动设置")
			}
		}
		if stateErr != nil {
			rollbackErr := removeManagedCreation(path, createdInfo, desired)
			reloadErr := backend.Reload(ctx)
			return nil, fmt.Errorf("安装后校验失败：%v；单元回滚=%v；管理器重载=%v", stateErr, rollbackErr, reloadErr)
		}
		keepRuntime = true
		return map[string]interface{}{"installed": true, "changed": true, "started": false, "enabled": false, "unitPath": path, "runtimeDirectory": o.Spec.RuntimeDirectory}, nil
	}
	_, original, e := files.read(path)
	if e != nil {
		return nil, e
	}
	state, e := backend.State(ctx)
	if e != nil {
		return nil, e
	}
	if e = checkSystemdOwnership(path, state); e != nil {
		return nil, e
	}

	switch o.Action {
	case "enable", "disable":
		if e = backend.SetEnabled(ctx, o.Action == "enable"); e != nil {
			return nil, e
		}
		if e = backend.Reload(ctx); e != nil {
			return nil, e
		}
	case "start", "stop", "restart":
		if e = backend.Job(ctx, o.Action); e != nil {
			return nil, e
		}
	case "uninstall":
		active, _ := state["ActiveState"].(string)
		if active != "inactive" && active != "failed" {
			return nil, errors.New("服务尚在运行或转换中，请先执行 webhook service stop")
		}
		if e = backend.SetEnabled(ctx, false); e != nil {
			return nil, e
		}
		current, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		if string(current) != string(original) {
			return nil, errors.New("服务单元发生并发修改，拒绝删除")
		}
		if e = os.Remove(path); e != nil {
			return nil, e
		}
		if e = backend.Reload(ctx); e != nil {
			return nil, fmt.Errorf("单元已移除，但 systemd 重载未确认：%w", e)
		}
		return map[string]interface{}{"uninstalled": true, "configurationPreserved": true}, nil
	}
	refreshed, e := backend.State(ctx)
	if e != nil {
		return nil, e
	}
	active, _ := refreshed["ActiveState"].(string)
	if (o.Action == "start" || o.Action == "restart") && active != "active" {
		return nil, fmt.Errorf("服务启动任务结束，但当前状态为 %s；请检查配置和日志", active)
	}
	if o.Action == "stop" && active != "inactive" && active != "failed" {
		return nil, fmt.Errorf("服务停止未确认，当前状态为 %s", active)
	}
	if o.Action == "enable" || o.Action == "disable" {
		expected := "disabled"
		if o.Action == "enable" {
			expected = "enabled"
		}
		if refreshed["UnitFileState"] != expected {
			return nil, fmt.Errorf("开机启动设置未确认，当前状态为 %v", refreshed["UnitFileState"])
		}
	}
	return map[string]interface{}{"action": o.Action, "service": managedServiceName, "activeState": refreshed["ActiveState"], "subState": refreshed["SubState"], "unitFileState": refreshed["UnitFileState"]}, nil
}
func checkSystemdOwnership(path string, state map[string]interface{}) error {
	fragment, _ := state["FragmentPath"].(string)
	if filepath.Clean(fragment) != filepath.Clean(path) {
		return errors.New("systemd 加载的单元路径与受管文件不同")
	}
	if drops, ok := state["DropInPaths"].([]string); ok && len(drops) > 0 {
		return errors.New("服务存在 drop-in 配置，拒绝自动接管")
	}
	if triggered, ok := state["TriggeredBy"].([]string); ok && len(triggered) > 0 {
		return errors.New("服务存在 socket 等关联触发单元，拒绝自动接管")
	}
	return nil
}

func removeManagedCreation(path string, info os.FileInfo, data []byte) error {
	if info == nil {
		return errors.New("无法确认本次创建的单元身份")
	}
	current, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !os.SameFile(info, current) {
		return errors.New("单元被外部替换，保留现有文件")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if !bytes.Equal(raw, data) {
		return errors.New("单元被外部编辑，保留现有文件")
	}
	return os.Remove(path)
}
