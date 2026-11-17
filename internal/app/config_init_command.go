package app

import (
	"bytes"
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

func runConfigCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprintln(stdout, "用法：webhook config init --dir 目录 [--profile global|hub|agent|both] [--format yaml|json] [--dry-run]\n      webhook config template list\n生成最小本地配置或分类参考模板，不启动或注册服务。")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] == "template" {
		return runConfigTemplateCommand(args[1:], stdout, stderr)
	}
	if args[0] != "init" {
		fmt.Fprintln(stderr, "未知 config 子命令")
		return 2
	}
	fs := flag.NewFlagSet("webhook config init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setCommandHelp(fs, "在新目录或空目录生成最小配置；拒绝覆盖已有文件。")
	target := fs.String("dir", "", "目标目录（必填）")
	profile := fs.String("profile", "global", "场景：global、hub、agent、both")
	format := fs.String("format", "yaml", "格式：yaml（带注释）或 json（无注释）")
	dry := fs.Bool("dry-run", false, "只预览，不创建目录或文件")
	_ = fs.Bool("json", false, "输出 JSON")
	if e := fs.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *target == "" || fs.NArg() != 0 || (*profile != "global" && *profile != "hub" && *profile != "agent" && *profile != "both") || (*format != "yaml" && *format != "json") {
		fmt.Fprintln(stderr, "需要 --dir 目录；profile 为 global/hub/agent/both，format 为 yaml/json")
		return 2
	}
	fail := func(e error) int { fmt.Fprintln(stderr, e); return 1 }
	dir, e := filepath.Abs(*target)
	if e != nil {
		return fail(e)
	}
	exists := false
	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fail(errors.New("目标必须是普通目录"))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fail(err)
		}
		if len(entries) > 0 {
			return fail(errors.New("目标目录非空，拒绝覆盖"))
		}
		exists = true
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	configuration, e := templateYAML(*profile)
	if e != nil {
		return fail(e)
	}
	configName := "webhook." + *format
	hooksName := "hooks." + *format
	quote := func(s string) string { raw, _ := json.Marshal(s); return string(raw) }
	text := strings.ReplaceAll(string(configuration), `"./hooks.yaml"`, quote(filepath.Join(dir, hooksName)))
	text = strings.ReplaceAll(text, `"./.runtime"`, quote(filepath.Join(dir, ".runtime")))
	configuration = []byte(text)
	hooksData := []byte(`# 初始为空，不附带任何可执行命令。
# 生成单个 Hook：webhook config template hook --output hook.yaml
# 编辑后通过 hooks add --from hook.yaml 添加；完整功能示例：config template hooks。
[]
`)
	if *format == "json" {
		raw, err := yaml.YAMLToJSON(configuration)
		if err != nil {
			return fail(err)
		}
		var pretty bytes.Buffer
		if err = json.Indent(&pretty, raw, "", "  "); err != nil {
			return fail(err)
		}
		configuration = append(pretty.Bytes(), '\n')
		hooksData = []byte("[]\n")
	}
	instructions := fmt.Sprintf("配置场景：%s\n启动：webhook --config %s\n初始 Hook 集合为空，/status 可能显示 degraded。\n列出所有配置模板及用途：webhook config template list\n新增 Hook：先生成 hook 模板，编辑后通过 hooks add --config %s --file %s --from Hook对象文件 添加。\nHub/Agent 需先配置注释中指定的高熵令牌和真实地址；生成模板不会自动授权或启动。\n配置使用绝对路径，搬迁目录后需更新路径。\n不自动启动服务、不安装系统服务；参考功能说明已嵌入 YAML 注释，JSON 不含注释。\n", *profile, configName, configName, hooksName)
	artifacts := map[string][]byte{configName: configuration, hooksName: hooksData, "README.txt": []byte(instructions)}
	result := map[string]interface{}{"directory": dir, "profile": *profile, "format": *format, "dryRun": *dry, "created": !*dry, "files": []string{configName, hooksName, "README.txt"}, "configuration": string(configuration)}
	if *dry {
		return writeManagementJSON(stdout, stderr, result)
	}
	if !exists {
		if e = secureRuntimeDirectory(dir, true); e != nil {
			_ = os.Remove(dir)
			return fail(e)
		}
	}
	type created struct {
		path string
		info os.FileInfo
		data []byte
	}
	made := []created{}
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, f := range made {
			info, e := os.Lstat(f.path)
			if e != nil || !os.SameFile(info, f.info) {
				continue
			}
			raw, e := os.ReadFile(f.path)
			if e == nil && bytes.Equal(raw, f.data) {
				_ = os.Remove(f.path)
			}
		}
		if !exists {
			_ = os.Remove(dir)
		}
	}()
	for _, name := range []string{hooksName, configName, "README.txt"} {
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return fail(err)
		}
		data := artifacts[name]
		written, writeErr := f.Write(data)
		err = writeErr
		if err == nil {
			err = f.Sync()
		}
		info, statErr := f.Stat()
		closeErr := f.Close()
		if statErr == nil {
			made = append(made, created{path, info, data[:written]})
		}
		if err != nil {
			return fail(err)
		}
		if statErr != nil {
			return fail(statErr)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
	}
	committed = true
	return writeManagementJSON(stdout, stderr, result)
}
