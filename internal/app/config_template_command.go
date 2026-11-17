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
	{"hub", "完整 Hub 服务配置：节点授权与公开路由", "webhook --config hub.yaml"},
	{"agent", "完整 Agent 服务配置：通过 Hub 执行本地 Hook", "webhook --config agent.yaml"},
	{"both", "完整级联服务配置：下游 Hub 与上游 Agent 共存", "webhook --config both.yaml"},
	{"network-hub", "独立 Hub 组网配置；不是服务总配置", "通过总配置 network-config 引用"},
	{"network-agent", "独立 Agent 组网配置；不是服务总配置", "通过总配置 network-config 引用"},
	{"network-both", "独立级联组网配置；不是服务总配置", "通过总配置 network-config 引用"},
	{"admin", "Admin/TOTP/JWT 配置：默认关闭，启用前替换密钥", "webhook --config admin.yaml"},
	{"tls", "HTTPS 服务配置：先提供真实证书与私钥", "webhook --config tls.yaml"},
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
		global, e := templateYAML("global")
		if e != nil {
			return nil, e
		}
		net, e := templateYAML("network-" + name)
		if e != nil {
			return nil, e
		}
		lines := strings.Split(string(net), "\n")
		for len(lines) > 0 && (strings.HasPrefix(lines[0], "#") || strings.TrimSpace(lines[0]) == "") {
			lines = lines[1:]
		}
		if name == "both" {
			global = []byte(strings.Replace(string(global), "port: 9000\n", "port: 9001\n", 1))
		}
		return []byte(string(global) + "\n# 以下为内联组网配置；凭据需在启动前准备，不能再同时设置 network-config。\nnetwork:\n" + indentTemplate(strings.Join(lines, "\n"))), nil
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
	return configurationTemplates.ReadFile("configtemplates/" + name + ".yaml")
}
func indentTemplate(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n  ") + "\n"
}
func renderConfigTemplate(name, format string) ([]byte, error) {
	data, e := templateYAML(name)
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
		fmt.Fprintln(stdout, "用法：webhook config template list [--json]\n      webhook config template <名称> [--format yaml|json] [--output 路径]\n默认输出带中文注释的 YAML；JSON 无注释。先查看 list 选择用途，生成后按注释填写，不会执行或自动应用配置。")
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
	if e := fs.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "多余的位置参数")
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
	data, e := renderConfigTemplate(args[0], *format)
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
