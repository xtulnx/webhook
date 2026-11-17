package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ghodss/yaml"
	"github.com/xtulnx/webhook/internal/hook"
	yamlv2 "gopkg.in/yaml.v2"
)

func runHookWriteCommand(action string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webhook hooks "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	setCommandHelp(fs, "修改已有 Hook 文件，保存前备份；可选择保存后主动刷新，并等待运行实例的回执。")
	var o localHookOptions
	var target, from, reload string
	var dry bool
	var runtimeDir, instance string
	fs.StringVar(&runtimeDir, "runtime-dir", "", "显式指定本机运行目录")
	fs.StringVar(&instance, "instance", "", "多个匹配实例时指定 ID")
	reloadTimeout := fs.Duration("reload-timeout", 5*time.Second, "实例发现与刷新回执的超时")
	fs.StringVar(&o.Config, "config", "", "总配置文件（写入配置时必填）")
	fs.StringVar(&target, "file", "", "配置集合内已有目标文件（写入配置时必填）")
	fs.StringVar(&o.Directory, "working-directory", "", "配置内路径及目标文件的基准目录")
	fs.StringVar(&o.ID, "id", "", "修改或删除的 Hook ID")
	fs.StringVar(&from, "from", "", "包含单个完整 Hook 对象的 JSON/YAML 文件")
	fs.BoolVar(&dry, "dry-run", false, "预览变更，不创建锁、备份或写入文件")
	fs.BoolVar(&o.JSON, "json", false, "输出 JSON 结果")
	fs.StringVar(&reload, "reload", "auto", "生效模式：auto、required 或 never")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || o.Config == "" || target == "" || (action != "add" && o.ID == "") || (action == "add" && o.ID != "") || (action == "delete" && from != "") || (action != "delete" && from == "") {
		fmt.Fprintln(stderr, "需要 config/file；add/update 需要 from，update/delete 需要 id；生成配置请用 config template")
		return 2
	}

	if (reload != "never" && reload != "auto" && reload != "required") || *reloadTimeout <= 0 {
		fmt.Fprintln(stderr, "reload 需要 auto/required/never，超时必须为正")
		return 2
	}
	fail := func(e error) int { fmt.Fprintln(stderr, e); return 1 }
	var updated hook.Hook

	configData, err := os.ReadFile(o.Config)
	if err != nil {
		return fail(err)
	}
	var cfg map[string]interface{}
	if err = yaml.Unmarshal(configData, &cfg); err != nil {
		return fail(errors.New("总配置解析失败"))
	}
	if templ, ok := cfg["template"]; ok {
		if templ != false {
			return fail(errAdminReadOnly)
		}
	}
	base, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	if o.Directory != "" {
		base, err = filepath.Abs(o.Directory)
		if err != nil {
			return fail(err)
		}
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	target = filepath.Clean(target)
	if !isHooksConfigFile(target) {
		return fail(errors.New("目标必须是 JSON/YAML Hook 文件"))
	}
	if action != "delete" {
		raw, e := readTestInput(from)
		if e != nil {
			return fail(e)
		}
		var syntax interface{}
		if e = yamlv2.UnmarshalStrict(raw, &syntax); e != nil {
			return fail(errors.New("from 配置格式无效或包含重复键"))
		}
		raw, e = yaml.YAMLToJSON(raw)
		if e != nil {
			return fail(errors.New("from 配置格式无效"))
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if e = dec.Decode(&updated); e != nil {
			return fail(errors.New("from 必须是完整 Hook 对象，不能包含未知字段"))
		}
		if e = dec.Decode(new(interface{})); e != io.EOF {
			return fail(errors.New("from 只能包含一个 Hook 对象"))
		}
		updated = normalizeAdminHook(updated)
		if updated.ID == "" {
			return fail(errors.New("Hook ID 不能为空"))
		}
		if action == "update" && updated.ID != o.ID {
			return fail(errors.New("update 不允许修改 Hook ID"))
		}
		if err = updated.ValidateExecutionSettings(); err != nil {
			return fail(errors.New("Hook 执行限制无效"))
		}
	}
	files, err := readLocalHooks(o)
	if err != nil {
		return fail(err)
	}
	var selection runtimeSelection
	if !dry && reload != "never" {
		ctx, cancel := context.WithTimeout(context.Background(), *reloadTimeout)
		selection, err = selectRuntime(ctx, o, runtimeDir, instance)
		cancel()
		if err != nil {
			return fail(err)
		}
		if reload == "required" && selection.record == nil {
			return fail(errors.New("未发现可刷新的运行实例，未保存"))
		}
	}
	paths := make([]string, 0, len(files)+1)
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	paths = append(paths, o.Config)
	var unlock func()
	if !dry {
		var e error
		unlock, e = lockHookDirectories(paths)
		if e != nil {
			return fail(e)
		}
		defer func() {
			if unlock != nil {
				unlock()
			}
		}()
	}
	latest, err := readLocalHooks(o)
	if err != nil {
		return fail(err)
	}
	if !sameLocalHookPaths(files, latest) {
		return fail(errors.New("Hook 来源集合发生变化，请重试"))
	}
	snapshots := make([]hookDiskSnapshot, 0, len(latest))
	candidate := map[string]hook.Hooks{}
	var current hook.Hooks
	found := false
	for _, f := range latest {
		s, e := readHookSnapshot(f.Path)
		if e != nil {
			return fail(e)
		}
		snapshots = append(snapshots, s)
		candidate[f.Path] = s.hooks
		if f.Path == target {
			current = s.hooks
			found = true
		}
	}
	if !found {
		return fail(fmt.Errorf("目标文件不属于指定配置集合：%s", target))
	}
	next := cloneHooks(current)
	if next == nil {
		next = hook.Hooks{}
	}
	index := hookIndexByID(next, o.ID)
	switch action {
	case "add":
		if hookIndexByID(next, updated.ID) >= 0 {
			return fail(errors.New("Hook ID 已存在"))
		}
		next = append(next, updated)
	case "update":
		if index < 0 {
			return fail(errors.New("未找到待修改 Hook"))
		}
		next[index] = updated
	case "delete":
		if index < 0 {
			return fail(errors.New("未找到待删除 Hook"))
		}
		next = append(next[:index], next[index+1:]...)
	}
	candidate[target] = next
	if err = validateUniqueHookIDs(candidate); err != nil {
		return fail(err)
	}
	checkConfig := func() error {
		raw, e := os.ReadFile(o.Config)
		if e != nil {
			return e
		}
		if !bytes.Equal(raw, configData) {
			return errors.New("总配置发生变化，请重试")
		}
		latest, e := readLocalHooks(o)
		if e != nil {
			return e
		}
		if !sameLocalHookPaths(files, latest) {
			return errors.New("Hook 来源集合发生变化，请重试")
		}
		return nil
	}
	if err = checkConfig(); err != nil {
		return fail(err)
	}
	backup := ""
	if !dry {
		backup, err = commitHookSnapshot(snapshots, target, next)
		if err != nil {
			return fail(err)
		}
	}
	id := o.ID
	if action == "add" {
		id = updated.ID
	}
	result := map[string]interface{}{"action": action, "id": id, "file": target, "dryRun": dry, "saved": !dry, "reload": "not-requested", "runtimeConfirmed": false, "beforeCount": len(current), "afterCount": len(next), "notice": "保存会重新序列化，注释及排版可能丢失；已有 hotreload 仍可能自动生效。"}
	if backup != "" {
		result["backup"] = backup
	}
	if unlock != nil {
		unlock()
		unlock = nil
	}
	code := 0
	if !dry && reload != "never" {
		if selection.record == nil {
			result["reload"] = "not-found"
		} else {
			raw, e := marshalHooksFile(target, next)
			if e != nil {
				return fail(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), *reloadTimeout)
			response, e := requestRuntimeReload(ctx, selection, latest, map[string]string{target: hashRuntimeBytes(raw)})
			cancel()
			if e != nil {
				result["reload"] = "unconfirmed"
				result["diagnostic"] = e.Error()
				code = 3
			} else {
				result["reload"] = "applied"
				result["runtimeConfirmed"] = true
				result["instance"] = response.Instance.ID
				result["generation"] = response.Generation
			}
		}
	}
	if writeManagementJSON(stdout, stderr, result) != 0 {
		return 1
	}
	return code
}

func sameLocalHookPaths(a, b []localHookFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path {
			return false
		}
	}
	return true
}
