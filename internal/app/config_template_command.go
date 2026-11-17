package app

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ghodss/yaml"
)

//go:embed configtemplates/*.yaml
var configurationTemplates embed.FS

type configTemplateInfo struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Use     string `json:"use"`
}

var templateCatalog = []configTemplateInfo{
	{"global", "全局配置：本机监听、来源/执行/日志及其他功能索引", "webhook --config webhook.yaml"},
	{"hook", "单个 Hook 对象：基础字段、规则、参数、上传及环境映射", "webhook hooks add --config webhook.yaml --file hooks.yaml --from hook.yaml"},
	{"hooks", "Hook 数组文件：用于服务直接加载", "webhook -hooks hooks.yaml"},
	{"hub", "Hub 模块：仅 network 节点授权与公开路由", "与 global/admin/tls 组合，或合入已有总配置"},
	{"agent", "Agent 模块：仅 network，通过 Hub 执行本地 Hook", "与 global/admin/tls 组合，或合入已有总配置"},
	{"both", "级联模块：仅 network，下游 Hub 与上游 Agent 共存", "与 global/admin/tls 组合；按需调整本机监听端口"},
	{"network-hub", "独立 Hub 组网配置；不是服务总配置", "通过总配置 network-config 引用"},
	{"network-agent", "独立 Agent 组网配置；不是服务总配置", "通过总配置 network-config 引用"},
	{"network-both", "独立级联组网配置；不是服务总配置", "通过总配置 network-config 引用"},
	{"admin", "Admin/TOTP/JWT 配置：默认关闭，启用前替换密钥", "与 global/hub/agent/both/tls 组合，或合入已有总配置"},
	{"tls", "TLS 模块：仅 HTTPS 字段，先提供真实证书与私钥", "与 global/hub/agent/both/admin 组合；监听端口由 global 设置"},
	{"context", "本地 test/explain 上下文：不是真实网络请求", "webhook hooks explain --config webhook.yaml --id example --context context.yaml"},
}

func templateYAML(name string) ([]byte, error) {
	switch name {
	case "hooks":
		data, e := templateYAML("hook")
		if e != nil {
			return nil, e
		}
		return []byte("# Hook 数组文件，适用于 -hooks 或总配置 hooks；请编辑命令路径。\n-\n" + indentTemplate(string(data))), nil
	case "hub", "agent", "both":
		net, e := templateYAML("network-" + name)
		if e != nil {
			return nil, e
		}
		lines := strings.Split(string(net), "\n")
		for len(lines) > 0 && (strings.HasPrefix(lines[0], "#") || strings.TrimSpace(lines[0]) == "") {
			lines = lines[1:]
		}
		return []byte("# 组网模块：合入总配置，或与 global/admin/tls 一起生成。\n# 凭据需在启动前准备，不能再同时设置 network-config。\nnetwork:\n" + indentTemplate(strings.Join(lines, "\n"))), nil
	}
	known := false
	for _, item := range templateCatalog {
		if item.Name == name {
			known = true
			break
		}
	}
	if !known {
		return nil, fmt.Errorf("未知模板 %q；使用 webhook config template list 查看用途", name)
	}
	data, err := configurationTemplates.ReadFile("configtemplates/" + name + ".yaml")
	if err != nil {
		return nil, err
	}
	// Embedded templates may come from a Git checkout with CRLF line endings.
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), nil
}
func indentTemplate(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n  ") + "\n"
}
func renderConfigTemplate(name, format string) ([]byte, error) {
	return renderConfigTemplates([]string{name}, format)
}

// Compose only modules sharing the service configuration schema. Keep source
// comments and a canonical order; reject conflicts rather than silently overriding.
func composeTemplateYAML(names []string) ([]byte, error) {
	if len(names) == 0 {
		return nil, errors.New("请指定至少一个模板名称")
	}
	selected := make(map[string]bool)
	mode := ""
	for _, name := range names {
		if selected[name] {
			return nil, fmt.Errorf("重复模板 %q；每个模块只需指定一次", name)
		}
		selected[name] = true
		switch name {
		case "hub", "agent", "both":
			if mode != "" {
				return nil, errors.New("hub、agent、both 互斥；级联场景请选择 both")
			}
			mode = name
		case "global", "admin", "tls":
		default:
			data, e := templateYAML(name)
			if e != nil {
				return nil, e
			}
			if len(names) > 1 {
				return nil, fmt.Errorf("%s 使用独立文件结构，不能与其他模板组合；可组合模块为 global、hub/agent/both、admin、tls", name)
			}
			return data, nil
		}
	}
	var result bytes.Buffer
	keys := make(map[string]string)
	for _, name := range []string{"global", "hub", "agent", "both", "admin", "tls"} {
		if !selected[name] {
			continue
		}
		data, e := templateYAML(name)
		if e != nil {
			return nil, e
		}
		var fields map[string]interface{}
		if e := yaml.Unmarshal(data, &fields); e != nil {
			return nil, e
		}
		for key := range fields {
			if previous, exists := keys[key]; exists {
				return nil, fmt.Errorf("模板 %s 与 %s 的字段 %s 冲突", previous, name, key)
			}
			keys[key] = name
		}
		if result.Len() > 0 {
			result.WriteByte('\n')
		}
		result.Write(data)
	}
	return result.Bytes(), nil
}

func renderConfigTemplates(names []string, format string) ([]byte, error) {
	data, e := composeTemplateYAML(names)
	if e != nil {
		return nil, e
	}
	switch format {
	case "yaml":
		return data, nil
	case "json":
		raw, e := yaml.YAMLToJSON(data)
		if e != nil {
			return nil, e
		}
		var out bytes.Buffer
		if e = json.Indent(&out, raw, "", "  "); e != nil {
			return nil, e
		}
		return append(out.Bytes(), '\n'), nil
	default:
		return nil, errors.New("format 仅支持 yaml 或 json")
	}
}
func runConfigTemplateCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprintln(stdout, "用法：webhook config template list [--json]\n      webhook config template <模块> [模块...] [--format yaml|json] [--output 路径]\n默认输出带中文注释的 YAML；JSON 无注释。子模块仅生成目标字段。可组合 global、hub/agent/both（三选一）、admin、tls；其余模板单独生成。模块名放在选项之前。\n示例：webhook config template global hub admin --output webhook.yaml\n生成后按注释填写，不会执行或自动应用配置。")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] == "list" {
		fs := flag.NewFlagSet("webhook config template list", flag.ContinueOnError)
		fs.SetOutput(stderr)
		setCommandHelp(fs, "列出模板类型、用途和使用方式。")
		jsonOut := fs.Bool("json", false, "输出 JSON 目录")
		if e := fs.Parse(args[1:]); e != nil {
			if errors.Is(e, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, "list 不接受位置参数")
			return 2
		}
		if *jsonOut {
			return writeManagementJSON(stdout, stderr, templateCatalog)
		}
		for _, t := range templateCatalog {
			if _, e := fmt.Fprintf(stdout, "%s\t%s\n  %s\n", t.Name, t.Purpose, t.Use); e != nil {
				fmt.Fprintln(stderr, e)
				return 1
			}
		}
		return 0
	}
	fs := flag.NewFlagSet("webhook config template "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	setCommandHelp(fs, "生成参考模板；stdout 仅输出配置，写文件时拒绝覆盖已有路径。")
	format := fs.String("format", "yaml", "输出格式：yaml（带注释）或 json（无注释）")
	output := fs.String("output", "", "输出文件；留空或 - 输出到终端，不修改现有运行配置")
	moduleEnd := 0
	for moduleEnd < len(args) && !strings.HasPrefix(args[moduleEnd], "-") {
		moduleEnd++
	}
	names := args[:moduleEnd]
	if e := fs.Parse(args[moduleEnd:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "请将所有模块名放在 --format/--output 等选项之前")
		return 2
	}
	explicitFormat := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "format" {
			explicitFormat = true
		}
	})
	extension := strings.ToLower(filepath.Ext(*output))
	inferred := ""
	if extension == ".json" {
		inferred = "json"
	} else if extension == ".yaml" || extension == ".yml" {
		inferred = "yaml"
	}
	if inferred != "" {
		if explicitFormat && *format != inferred {
			fmt.Fprintln(stderr, "format 与输出文件扩展名不一致")
			return 2
		}
		*format = inferred
	}
	if *format != "yaml" && *format != "json" {
		fmt.Fprintln(stderr, "format 仅支持 yaml 或 json")
		return 2
	}
	data, e := renderConfigTemplates(names, *format)
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 2
	}
	if *output == "" || *output == "-" {
		if _, e = stdout.Write(data); e != nil {
			fmt.Fprintln(stderr, e)
			return 1
		}
		return 0
	}
	if e = writeNewConfigTemplate(*output, data); e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	fmt.Fprintf(stderr, "已生成 %s；请按注释或模板目录说明填写后再使用。\n", *output)
	return 0
}
func writeNewConfigTemplate(path string, data []byte) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	info, statErr := f.Stat()
	n, writeErr := f.Write(data)
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
		if current, e := os.Lstat(path); e == nil && info != nil && os.SameFile(info, current) {
			raw, e := os.ReadFile(path)
			if e == nil && bytes.Equal(raw, data[:n]) {
				_ = os.Remove(path)
			}
		}
	}
	return writeErr
}
