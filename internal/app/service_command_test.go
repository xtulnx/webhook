package app

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSystemd struct {
	path        string
	state       map[string]interface{}
	reloadError error
	loadState   string
	jobs        []string
	enabled     bool
	reloads     int
}

func (f *fakeSystemd) State(context.Context) (map[string]interface{}, error) { return f.state, nil }
func (f *fakeSystemd) Reload(context.Context) error {
	f.reloads++
	if f.reloadError != nil {
		return f.reloadError
	}
	if _, e := os.Stat(f.path); e == nil {
		f.state = map[string]interface{}{"FragmentPath": f.path, "LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "UnitFileState": "disabled"}
		if f.loadState != "" {
			f.state["LoadState"] = f.loadState
		}
		if f.enabled {
			f.state["UnitFileState"] = "enabled"
		}
	}
	return nil
}
func (f *fakeSystemd) SetEnabled(_ context.Context, v bool) error { f.enabled = v; return nil }
func (f *fakeSystemd) Job(_ context.Context, action string) error {
	f.jobs = append(f.jobs, action)
	if action == "stop" {
		f.state["ActiveState"] = "inactive"
	} else {
		f.state["ActiveState"] = "active"
	}
	return nil
}
func testSystemdFiles() systemdFiles {
	return systemdFiles{read: func(path string) (serviceInstallSpec, []byte, error) {
		var s serviceInstallSpec
		info, e := os.Lstat(path)
		if e != nil {
			return s, nil, e
		}
		if !info.Mode().IsRegular() {
			return s, nil, errors.New("not a regular unit")
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return s, nil, e
		}
		s, e = parseManagedUnit(raw)
		return s, raw, e
	}, prepareRuntime: func(serviceInstallSpec) (func(), error) { return func() {}, nil }}
}
func testServiceSpec() serviceInstallSpec {
	return serviceInstallSpec{Executable: "/opt/webhook/webhook", Config: "/opt/webhook/config.yaml", Directory: "/opt/webhook", User: "webhook", RuntimeDirectory: "/run/webhook"}
}

func TestManagedServiceLifecyclePolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "webhook.service")
	s := testServiceSpec()
	backend := &fakeSystemd{path: path, state: map[string]interface{}{"LoadState": "not-found"}}
	call := func(action string) (interface{}, error) {
		return manageSystemd(context.Background(), backend, path, serviceOptions{Action: action, Spec: s}, testSystemdFiles())
	}
	if _, e := call("install"); e != nil {
		t.Fatal(e)
	}
	if len(backend.jobs) != 0 || backend.enabled {
		t.Fatal("install started or enabled service")
	}
	if _, e := call("install"); e != nil {
		t.Fatal("idempotence", e)
	}
	if _, e := call("start"); e != nil {
		t.Fatal(e)
	}
	if _, e := call("uninstall"); e == nil {
		t.Fatal("running service uninstalled")
	}
	if _, e := call("stop"); e != nil {
		t.Fatal(e)
	}
	if _, e := call("enable"); e != nil || !backend.enabled {
		t.Fatal("enable failed", e)
	}
	if _, e := call("uninstall"); e != nil {
		t.Fatal(e)
	}
	if backend.enabled {
		t.Fatal("uninstall left unit enabled")
	}
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("unit not removed")
	}
}
func TestServiceInstallRejectsConflictsAndRollsBack(t *testing.T) {
	for _, scenario := range []string{"foreign", "dropin", "managerConflict", "reloadFailure", "badSetting"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "webhook.service")
			backend := &fakeSystemd{path: path, state: map[string]interface{}{"LoadState": "not-found"}}
			switch scenario {
			case "foreign":
				if e := os.WriteFile(path, []byte("[Service]\nExecStart=/foreign\n"), 0644); e != nil {
					t.Fatal(e)
				}
			case "dropin":
				if e := os.Mkdir(path+".d", 0755); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(path+".d", "override.conf"), []byte("data"), 0644); e != nil {
					t.Fatal(e)
				}
			case "managerConflict":
				backend.state["LoadState"] = "loaded"
			case "reloadFailure":
				backend.reloadError = errors.New("injected failure")
			case "badSetting":
				backend.loadState = "bad-setting"
			}
			_, e := manageSystemd(context.Background(), backend, path, serviceOptions{Action: "install", Spec: testServiceSpec()}, testSystemdFiles())
			if e == nil {
				t.Fatal("conflict accepted")
			}
			if scenario == "foreign" {
				raw, _ := os.ReadFile(path)
				if !strings.Contains(string(raw), "/foreign") {
					t.Fatal("foreign unit changed")
				}
			} else if _, e := os.Stat(path); !os.IsNotExist(e) {
				t.Fatal("failed install left unit")
			}
		})
	}
}
func TestServiceStatusIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "webhook.service")
	raw, e := renderManagedUnit(testServiceSpec())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0644); e != nil {
		t.Fatal(e)
	}
	backend := &fakeSystemd{path: path, state: map[string]interface{}{"FragmentPath": path, "ActiveState": "inactive"}}
	if _, e = manageSystemd(context.Background(), backend, path, serviceOptions{Action: "status"}, testSystemdFiles()); e != nil {
		t.Fatal(e)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("status wrote lock or metadata")
	}
}
func TestServiceUnitExpansionAndTamperDetection(t *testing.T) {
	s := testServiceSpec()
	s.Directory = "/opt/webhook %data"
	s.Config = path.Join(s.Directory, "$CONFIG%name with spaces.yaml")
	raw, e := renderManagedUnit(s)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(raw), "$$CONFIG%%name with spaces.yaml") {
		t.Fatalf("unsafe expansion: %s", raw)
	}
	if !strings.Contains(string(raw), "\nUser=webhook\nWorkingDirectory=/opt/webhook %%data\n") {
		t.Fatalf("invalid literal settings: %s", raw)
	}
	legacy, e := renderServiceUnit(s, true)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = parseManagedUnit(legacy); e != nil {
		t.Fatalf("old unit cannot be managed after upgrade: %v", e)
	}
	if _, e = parseManagedUnit(append(legacy, []byte("Environment=INJECTED=yes\n")...)); e == nil {
		t.Fatal("changed old unit accepted")
	}
	if _, e = parseManagedUnit(raw); e != nil {
		t.Fatal(e)
	}
	if _, e = parseManagedUnit(append(raw, []byte("Environment=INJECTED=yes\n")...)); e == nil {
		t.Fatal("changed unit accepted")
	}
	s.Directory += "\nExecStart=/injected"
	if _, e = renderManagedUnit(s); e == nil {
		t.Fatal("newline injection accepted")
	}
}
