package app

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/xtulnx/webhook/internal/hook"
	"github.com/xtulnx/webhook/internal/network"
)

// Management commands load their own configuration and never initialize the server.
type localHookOptions struct {
	Config    string
	File      string
	Directory string
	Template  bool
	JSON      bool
	Secrets   bool
	ID        string
}

type localHookFile struct {
	Path  string     `json:"path"`
	Hooks hook.Hooks `json:"hooks"`
}

func runHooksCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, "用法：webhook hooks <list|show|validate|add|update|delete|reload|test|explain> [参数]\n查询与校验本地磁盘配置，不启动服务。")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	action := args[0]
	if action == "reload" {
		return runHooksReloadCommand(args[1:], stdout, stderr)
	}
	if action == "add" || action == "update" || action == "delete" {
		return runHookWriteCommand(action, args[1:], stdout, stderr)
	}
	if action == "test" || action == "explain" {
		return runHookTestCommand(action, args[1:], stdout, stderr)
	}
	if action != "list" && action != "show" && action != "validate" {
		fmt.Fprintf(stderr, "未知的 hooks 子命令 %q\n", action)
		return 2
	}
	flags := flag.NewFlagSet("webhook hooks "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	setCommandHelp(flags, "读取本地 Hook 配置；相对路径默认基于当前目录。")
	var o localHookOptions
	flags.StringVar(&o.Config, "config", "", "总配置文件，与 file 互斥")
	flags.StringVar(&o.File, "file", "", "单个 Hook 文件，与 config 互斥")
	flags.StringVar(&o.Directory, "working-directory", "", "配置中相对路径的基准目录")
	flags.BoolVar(&o.Template, "template", false, "按 Go 模板读取 Hook 文件")
	flags.BoolVar(&o.JSON, "json", false, "以 JSON 输出结果")
	flags.BoolVar(&o.Secrets, "show-secrets", false, "显示完整 Hook 定义，可能包含密钥")
	flags.StringVar(&o.ID, "id", "", "要查询的 Hook ID")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || (o.Config != "" && o.File != "") || (action == "show" && strings.TrimSpace(o.ID) == "") {
		fmt.Fprintln(stderr, "请指定有效参数；show 必须指定 --id，config 与 file 互斥")
		return 2
	}
	files, err := readLocalHooks(o)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if action == "validate" {
		count := 0
		for _, f := range files {
			count += len(f.Hooks)
		}
		if o.JSON {
			return writeManagementJSON(stdout, stderr, map[string]interface{}{"valid": true, "fileCount": len(files), "hookCount": count})
		}
		fmt.Fprintf(stdout, "校验通过：%d 个文件，%d 个 Hook（未执行命令）\n", len(files), count)
		return 0
	}
	if action == "show" {
		for _, f := range files {
			for _, h := range f.Hooks {
				if h.ID == o.ID {
					return writeManagementJSON(stdout, stderr, redactManagementHook(h, o.Secrets))
				}
			}
		}
		fmt.Fprintf(stderr, "未找到 Hook %q\n", o.ID)
		return 1
	}
	type entry struct {
		ID   string `json:"id"`
		File string `json:"file"`
	}
	entries := make([]entry, 0)
	for _, f := range files {
		for _, h := range f.Hooks {
			entries = append(entries, entry{h.ID, f.Path})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	if o.JSON {
		return writeManagementJSON(stdout, stderr, entries)
	}
	for _, e := range entries {
		fmt.Fprintf(stdout, "%s\t%s\n", e.ID, e.File)
	}
	return 0
}

func writeManagementJSON(stdout, stderr io.Writer, value interface{}) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		fmt.Fprintln(stderr, "输出失败:", err)
		return 1
	}
	return 0
}

func redactManagementHook(h hook.Hook, reveal bool) interface{} {
	data, _ := json.Marshal(h)
	var value interface{}
	_ = json.Unmarshal(data, &value)
	if reveal {
		return value
	}
	var visit func(interface{})
	visit = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, item := range x {
				lower := strings.ToLower(k)
				if strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "password") {
					x[k] = "[REDACTED]"
				} else {
					visit(item)
				}
			}
		case []interface{}:
			for _, item := range x {
				visit(item)
			}
		}
	}
	visit(value)
	return map[string]interface{}{"hook": value, "notice": "已隐藏已知密钥字段；其他字段仍可能含敏感数据。"}
}

func readLocalHooks(o localHookOptions) ([]localHookFile, error) {
	base, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if o.Directory != "" {
		base, err = filepath.Abs(o.Directory)
		if err != nil {
			return nil, err
		}
		info, e := os.Stat(base)
		if e != nil {
			return nil, e
		}
		if !info.IsDir() {
			return nil, errors.New("working-directory 必须是目录")
		}
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(base, p)
	}
	paths := []string{}
	dirs := []string{}
	template := o.Template
	if o.Config != "" {
		// The config argument itself is relative to the CLI directory, as in server startup.
		data, e := os.ReadFile(o.Config)
		if e != nil {
			return nil, e
		}
		var cfg map[string]interface{}
		if e = yaml.Unmarshal(data, &cfg); e != nil {
			return nil, fmt.Errorf("总配置解析失败: %w", e)
		}
		for key := range cfg {
			if flag.CommandLine.Lookup(key) == nil && key != "hooks" && key != "hooks-dir" && key != "header" && key != "network" && key != "socket" && key != "setuid" && key != "setgid" {
				return nil, fmt.Errorf("未知总配置选项 %q", key)
			}
		}
		extract := func(key string) ([]string, error) {
			v, ok := cfg[key]
			if !ok {
				return nil, nil
			}
			if s, ok := v.(string); ok {
				return []string{s}, nil
			}
			list, ok := v.([]interface{})
			if !ok {
				return nil, fmt.Errorf("%s 必须是路径或路径列表", key)
			}
			out := []string{}
			for _, v := range list {
				s, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("%s 包含非字符串路径", key)
				}
				out = append(out, s)
			}
			return out, nil
		}
		paths, err = extract("hooks")
		if err != nil {
			return nil, err
		}
		dirs, err = extract("hooks-dir")
		if err != nil {
			return nil, err
		}
		if v, ok := cfg["template"]; ok {
			b, ok := v.(bool)
			if !ok {
				return nil, errors.New("template 必须是布尔值")
			}
			template = template || b
		}
		if len(paths) == 0 && len(dirs) == 0 {
			var nc *network.Config
			embedded, hasEmbedded := cfg["network"]
			external, hasExternal := cfg["network-config"]
			if hasEmbedded && hasExternal {
				return nil, errors.New("不能同时指定 network 与 network-config")
			}
			if hasEmbedded {
				raw, e := yaml.Marshal(embedded)
				if e != nil {
					return nil, errors.New("无效的内联组网配置")
				}
				nc, err = network.ParseConfig(raw, filepath.Dir(o.Config))
			} else if hasExternal {
				name, ok := external.(string)
				if !ok || name == "" {
					return nil, errors.New("network-config 必须是路径")
				}
				if !filepath.IsAbs(name) {
					name = filepath.Join(filepath.Dir(o.Config), name)
				}
				nc, err = network.LoadConfig(name)
			}
			if err != nil {
				return nil, errors.New("组网配置校验失败，请检查来源、字段与凭据")
			}
			if nc != nil && (nc.Agent == nil || !nc.Agent.WithWebhook) {
				return []localHookFile{}, nil
			}
		}
	} else if o.File != "" {
		paths = append(paths, o.File)
	}
	if len(paths) == 0 && len(dirs) == 0 {
		paths = append(paths, "hooks.json")
	}
	failures := []error{}
	for _, d := range dirs {
		found, e := directoryHooksFiles(resolve(d))
		if e != nil {
			failures = append(failures, e)
		} else {
			paths = append(paths, found...)
		}
	}
	seen := map[string]bool{}
	ids := map[string]string{}
	out := []localHookFile{}
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			failures = append(failures, errors.New("Hook 路径不能为空"))
			continue
		}
		p = resolve(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		info, e := os.Stat(p)
		if e != nil {
			failures = append(failures, fmt.Errorf("%s: %w", p, e))
			continue
		}
		if !info.Mode().IsRegular() {
			failures = append(failures, fmt.Errorf("%s 不是普通文件", p))
			continue
		}
		var hs hook.Hooks
		if e = hs.LoadFromFileStrict(p, template); e != nil {
			failures = append(failures, fmt.Errorf("%s: 配置解析失败", p))
			continue
		}
		for _, h := range hs {
			if strings.TrimSpace(h.ID) == "" {
				failures = append(failures, fmt.Errorf("%s: Hook ID 不能为空", p))
			}
			if prev, ok := ids[h.ID]; ok {
				failures = append(failures, fmt.Errorf("Hook %q 重复定义于 %s 和 %s", h.ID, prev, p))
			}
			ids[h.ID] = p
		}
		out = append(out, localHookFile{p, hs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, errors.Join(failures...)
}
