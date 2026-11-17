package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ghodss/yaml"
)

const managedServiceName = "webhook.service"
const managedUnitPath = "/etc/systemd/system/webhook.service"
const serviceMarker = "# webhook-managed-v1 "

type serviceInstallSpec struct {
	Executable       string `json:"executable"`
	Config           string `json:"config"`
	Directory        string `json:"workingDirectory"`
	User             string `json:"user"`
	RuntimeDirectory string `json:"runtimeDirectory"`
	UID              int    `json:"-"`
	GID              int    `json:"-"`
}

type serviceOptions struct {
	Action  string
	Spec    serviceInstallSpec
	DryRun  bool
	Timeout time.Duration
}

func runServiceCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, "用法：webhook service <install|uninstall|start|stop|restart|status|enable|disable> [参数]\n原生管理本工具注册的系统服务。")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	action := args[0]
	switch action {
	case "install", "uninstall", "start", "stop", "restart", "status", "enable", "disable":
	default:
		fmt.Fprintln(stderr, "未知 service 子命令")
		return 2
	}
	fs := flag.NewFlagSet("webhook service "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	setCommandHelp(fs, "原生操作固定的 webhook 系统服务；不调用辅助脚本，不自动提权。")
	opts := serviceOptions{Action: action}
	_ = fs.Bool("json", false, "输出 JSON")
	fs.DurationVar(&opts.Timeout, "timeout", 15*time.Second, "等待系统服务操作的超时")
	if action == "install" {
		fs.StringVar(&opts.Spec.Config, "config", "", "总配置文件（必填）")
		fs.StringVar(&opts.Spec.Directory, "working-directory", "", "工作目录（必填）")
		fs.StringVar(&opts.Spec.User, "user", "", "已存在的运行用户（必填）")
		fs.BoolVar(&opts.DryRun, "dry-run", false, "预览单元与安装路径，不创建文件或连接系统服务管理器")
	}
	if e := fs.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || opts.Timeout <= 0 {
		fmt.Fprintln(stderr, "无效参数或超时")
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintln(stderr, "当前平台的原生服务管理尚未实现；本阶段仅支持 Linux systemd")
		return 1
	}
	if action == "install" {
		if opts.Spec.Config == "" || opts.Spec.Directory == "" || opts.Spec.User == "" {
			fmt.Fprintln(stderr, "install 需要 config、working-directory 和 user")
			return 2
		}
		spec, e := prepareServiceInstall(opts.Spec)
		if e != nil {
			fmt.Fprintln(stderr, e)
			return 1
		}
		opts.Spec = spec
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()
	result, e := servicePlatformOperation(ctx, opts)
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	return writeManagementJSON(stdout, stderr, result)
}

func prepareServiceInstall(s serviceInstallSpec) (serviceInstallSpec, error) {
	var e error
	s.Config, e = canonicalRuntimePath(s.Config)
	if e != nil {
		return s, e
	}
	s.Directory, e = canonicalRuntimePath(s.Directory)
	if e != nil {
		return s, e
	}
	info, e := os.Stat(s.Config)
	if e != nil || !info.Mode().IsRegular() {
		return s, errors.New("总配置必须是普通文件")
	}
	info, e = os.Stat(s.Directory)
	if e != nil || !info.IsDir() {
		return s, errors.New("工作目录必须是目录")
	}
	exe, e := os.Executable()
	if e != nil {
		return s, e
	}
	s.Executable, e = canonicalRuntimePath(exe)
	if e != nil {
		return s, e
	}
	if strings.ContainsAny(s.User, "\x00\r\n\t /\\%$") || s.User == "" {
		return s, errors.New("服务用户名称无效")
	}
	u, e := user.Lookup(s.User)
	if e != nil {
		return s, errors.New("服务用户不存在")
	}
	s.UID, e = strconv.Atoi(u.Uid)
	if e != nil {
		return s, e
	}
	s.GID, e = strconv.Atoi(u.Gid)
	if e != nil {
		return s, e
	}
	raw, e := os.ReadFile(s.Config)
	if e != nil {
		return s, e
	}
	var cfg map[string]interface{}
	if e = yaml.Unmarshal(raw, &cfg); e != nil {
		return s, errors.New("总配置解析失败")
	}
	s.RuntimeDirectory = "/run/webhook"
	if v, ok := cfg["runtime-dir"]; ok {
		value, ok := v.(string)
		if !ok {
			return s, errors.New("runtime-dir 必须是路径")
		}
		if value != "" {
			if !filepath.IsAbs(value) {
				value = filepath.Join(s.Directory, value)
			}
			s.RuntimeDirectory = filepath.Clean(value)
		}
	}
	if s.RuntimeDirectory != "/run/webhook" && (s.RuntimeDirectory == "/run" || strings.HasPrefix(s.RuntimeDirectory, "/run/")) {
		return s, errors.New("首版仅支持 /run/webhook 或持久目录中的自定义 runtime-dir")
	}
	// A readable and structurally valid hook set is an installation prerequisite.
	if _, e = readLocalHooks(localHookOptions{Config: s.Config, Directory: s.Directory}); e != nil {
		return s, e
	}
	if _, e = renderManagedUnit(s); e != nil {
		return s, e
	}
	return s, nil
}

// systemd performs its own specifier and environment expansion; shell quoting
// alone is not sufficient. Every argument is independently quoted and escaped.
func systemdValue(value string, argument bool) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("systemd 参数不能包含 NUL 或换行")
	}
	value = strings.ReplaceAll(value, "%", "%%")
	if argument {
		value = strings.ReplaceAll(value, "$", "$$")
	}
	return strconv.Quote(value), nil
}
func renderManagedUnit(s serviceInstallSpec) ([]byte, error) {
	for _, p := range []string{s.Config, s.Directory, s.Executable, s.RuntimeDirectory} {
		if !path.IsAbs(p) {
			return nil, errors.New("服务路径必须是绝对路径")
		}
	}
	wd, e := systemdValue(s.Directory, false)
	if e != nil {
		return nil, e
	}
	user, e := systemdValue(s.User, false)
	if e != nil {
		return nil, e
	}
	parts := []string{}
	for _, arg := range []string{s.Executable, "-config", s.Config, "-runtime-dir", s.RuntimeDirectory} {
		v, e := systemdValue(arg, true)
		if e != nil {
			return nil, e
		}
		parts = append(parts, v)
	}
	metadata, e := json.Marshal(s)
	if e != nil {
		return nil, e
	}
	body := serviceMarker + base64.StdEncoding.EncodeToString(metadata) + "\n[Unit]\nDescription=Webhook service\nAfter=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\nUser=" + user + "\nWorkingDirectory=" + wd + "\nExecStart=" + strings.Join(parts, " ") + "\nRestart=on-failure\nRestartSec=5\nNoNewPrivileges=true\n"
	if s.RuntimeDirectory == "/run/webhook" {
		body += "RuntimeDirectory=webhook\nRuntimeDirectoryMode=0700\n"
	}
	body += "\n[Install]\nWantedBy=multi-user.target\n"
	return []byte(body), nil
}
func parseManagedUnit(raw []byte) (serviceInstallSpec, error) {
	var s serviceInstallSpec
	line, _, _ := strings.Cut(string(raw), "\n")
	if !strings.HasPrefix(line, serviceMarker) {
		return s, errors.New("拒绝接管非本工具创建的服务")
	}
	data, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, serviceMarker))
	if e != nil {
		return s, errors.New("服务管理记录损坏")
	}
	if e = json.Unmarshal(data, &s); e != nil {
		return s, e
	}
	expected, e := renderManagedUnit(s)
	if e != nil {
		return s, e
	}
	if string(raw) != string(expected) {
		return s, errors.New("服务单元已被修改，拒绝覆盖或操作")
	}
	return s, nil
}
