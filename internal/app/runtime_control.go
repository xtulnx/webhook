package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ghodss/yaml"
	"github.com/xtulnx/webhook/internal/hook"
)

var runtimeDirectory = flag.String("runtime-dir", "", "可选的私有运行目录，用于本机实例发现与 Hook 刷新")
var hookApplyMu sync.Mutex
var runtimeCleanupMu sync.Mutex
var runtimeCleanup func()

type runtimeRecord struct {
	ID         string `json:"id"`
	PID        int    `json:"pid"`
	Config     string `json:"config"`
	Directory  string `json:"workingDirectory"`
	ConfigHash string `json:"configHash"`
}
type controlRequest struct {
	Operation string            `json:"operation"`
	ID        string            `json:"id"`
	Files     []string          `json:"files,omitempty"`
	Expected  map[string]string `json:"expected,omitempty"`
}
type controlResponse struct {
	Files      []string          `json:"files,omitempty"`
	Instance   runtimeRecord     `json:"instance"`
	Generation uint64            `json:"generation"`
	Digests    map[string]string `json:"digests,omitempty"`
	Error      string            `json:"error,omitempty"`
}

func canonicalRuntimePath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	p, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	return filepath.EvalSymlinks(p)
}
func hashRuntimeBytes(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func stopRuntimeControl() {
	runtimeCleanupMu.Lock()
	f := runtimeCleanup
	runtimeCleanup = nil
	runtimeCleanupMu.Unlock()
	if f != nil {
		f()
	}
}

func startRuntimeControl() (func(), error) {
	if *runtimeDirectory == "" {
		return func() {}, nil
	}
	dir, err := filepath.Abs(*runtimeDirectory)
	if err != nil {
		return nil, err
	}
	if err = secureRuntimeDirectory(dir, true); err != nil {
		return nil, err
	}
	cfg, err := canonicalRuntimePath(configFile)
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	cwd, err = canonicalRuntimePath(cwd)
	if err != nil {
		return nil, err
	}
	rec := runtimeRecord{PID: os.Getpid(), Config: cfg, Directory: cwd}
	if cfg != "" {
		raw, e := os.ReadFile(cfg)
		if e != nil {
			return nil, e
		}
		rec.ConfigHash = hashRuntimeBytes(raw)
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return nil, err
	}
	rec.ID = hex.EncodeToString(random)
	listener, err := listenRuntimeControl(dir, rec.ID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, rec.ID+".instance")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		listener.Close()
		removeRuntimeEndpoint(dir, rec.ID)
		return nil, err
	}
	err = json.NewEncoder(file).Encode(rec)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		listener.Close()
		removeRuntimeEndpoint(dir, rec.ID)
		os.Remove(path)
		return nil, err
	}
	var once sync.Once
	cleanup := func() { once.Do(func() { listener.Close(); os.Remove(path); removeRuntimeEndpoint(dir, rec.ID) }) }
	runtimeCleanupMu.Lock()
	runtimeCleanup = cleanup
	runtimeCleanupMu.Unlock()
	generation := uint64(0)
	go func() {
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				var req controlRequest
				if e := json.NewDecoder(io.LimitReader(c, 1<<20)).Decode(&req); e != nil {
					return
				}
				response := controlResponse{Instance: rec}
				hookApplyMu.Lock()
				defer hookApplyMu.Unlock()
				response.Generation = generation
				if req.ID != rec.ID {
					response.Error = "实例身份不匹配"
				} else {
					switch req.Operation {
					case "identity":
						response.Files, _ = runtimeSourcePaths()
					case "reload":
						if rec.Config != "" {
							raw, e := os.ReadFile(rec.Config)
							if e != nil || hashRuntimeBytes(raw) != rec.ConfigHash {
								response.Error = "总配置已更改，需要重启"
								break
							}
						}
						digests, e := reloadRuntimeHooks(req)
						if e != nil {
							response.Error = e.Error()
						} else {
							generation++
							response.Generation = generation
							response.Digests = digests
						}
					default:
						response.Error = "不支持的控制操作"
					}
				}
				_ = json.NewEncoder(c).Encode(response)
			}(conn)
		}
	}()
	return cleanup, nil
}

// Caller owns hookApplyMu. A failed candidate never partially replaces runtime hooks.
func reloadRuntimeHooks(req controlRequest) (map[string]string, error) {
	paths, err := runtimeSourcePaths()
	if err != nil {
		return nil, err
	}
	byDirectory := map[string]string{}
	for _, dir := range hooksDirectories {
		found, e := directoryHooksFiles(dir)
		if e != nil {
			return nil, e
		}
		for _, p := range found {
			byDirectory[p] = dir
		}
	}
	expectedFiles := append([]string(nil), req.Files...)
	sort.Strings(expectedFiles)
	if !equalRuntimePaths(paths, expectedFiles) {
		return nil, errors.New("运行实例与 CLI 的有效 Hook 来源不一致")
	}
	candidate := map[string]hook.Hooks{}
	digests := map[string]string{}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("Hook 来源不可读")
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, errors.New("Hook 来源不可读")
		}
		var hs hook.Hooks
		if *asTemplate {
			if err = hs.LoadFromFileStrict(p, true); err != nil {
				return nil, errors.New("Hook 模板解析失败")
			}
		} else if err = hs.LoadFromBytesStrict(raw); err != nil {
			return nil, errors.New("Hook 配置解析失败")
		}
		digests[p] = hashRuntimeBytes(raw)
		candidate[p] = hs
	}
	if err := validateUniqueHookIDs(candidate); err != nil {
		return nil, errors.New("Hook ID 为空或重复")
	}
	for p, hash := range req.Expected {
		if digests[p] != hash {
			return nil, errors.New("保存的 Hook 内容已发生变化，未确认生效")
		}
	}
	for _, p := range paths {
		raw, e := os.ReadFile(p)
		if e != nil || hashRuntimeBytes(raw) != digests[p] {
			return nil, errors.New("Hook 来源在加载期间发生变化")
		}
	}
	latest, sourceErr := runtimeSourcePaths()
	if sourceErr != nil || !equalRuntimePaths(paths, latest) {
		return nil, errors.New("Hook 来源集合在加载期间发生变化")
	}
	loadedHooksMu.Lock()
	defer loadedHooksMu.Unlock()
	loadedHooksFromFiles = candidate
	hooksFiles = append(hook.HooksFiles(nil), paths...)
	hookDirectoryByFile = byDirectory
	hookLoadMetadataByFile = map[string]hookLoadMetadata{}
	hookLoadFailures = map[string]hookLoadFailure{}
	now := time.Now().UTC()
	for _, p := range paths {
		markHookLoadedLocked(p, now)
	}
	return digests, nil
}
func equalRuntimePaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func runtimeControlCall(ctx context.Context, dir string, rec runtimeRecord, req controlRequest) (controlResponse, error) {
	var result controlResponse
	c, err := dialRuntimeControl(ctx, dir, rec.ID)
	if err != nil {
		return result, err
	}
	defer c.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	req.ID = rec.ID
	if err = json.NewEncoder(c).Encode(req); err != nil {
		return result, err
	}
	if err = json.NewDecoder(io.LimitReader(c, 1<<20)).Decode(&result); err != nil {
		return result, err
	}
	if result.Instance != rec {
		return result, errors.New("控制回执实例身份不匹配")
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

type runtimeSelection struct {
	dir    string
	record *runtimeRecord
}

func selectRuntime(ctx context.Context, o localHookOptions, explicit, instance string) (runtimeSelection, error) {
	var result runtimeSelection
	cfg, err := canonicalRuntimePath(o.Config)
	if err != nil {
		return result, err
	}
	directory, err := os.Getwd()
	if err != nil {
		return result, err
	}
	if o.Directory != "" {
		directory = o.Directory
	}
	directory, err = canonicalRuntimePath(directory)
	if err != nil {
		return result, err
	}
	raw, err := os.ReadFile(o.Config)
	if err != nil {
		return result, err
	}
	runtimeDir := explicit
	if runtimeDir == "" {
		runtimeDir, err = installedServiceRuntime(cfg)
		if err != nil {
			return result, err
		}
	}
	if runtimeDir == "" {
		var settings map[string]interface{}
		if err = yaml.Unmarshal(raw, &settings); err != nil {
			return result, errors.New("总配置无效")
		}
		if value, ok := settings["runtime-dir"]; ok {
			var valid bool
			runtimeDir, valid = value.(string)
			if !valid {
				return result, errors.New("runtime-dir 必须是路径")
			}
		}
	}
	if runtimeDir == "" {
		if instance != "" {
			return result, errors.New("指定 instance 需要运行目录")
		}
		return result, nil
	}
	if !filepath.IsAbs(runtimeDir) {
		runtimeDir = filepath.Join(directory, runtimeDir)
	}
	result.dir = runtimeDir
	if _, err = os.Lstat(runtimeDir); os.IsNotExist(err) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	if err = secureRuntimeDirectory(runtimeDir, false); err != nil {
		return result, err
	}
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".instance") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".instance")
		if !validRuntimeID(id) {
			return result, errors.New("运行目录包含无效实例记录")
		}
		if instance != "" && instance != id {
			continue
		}
		path := filepath.Join(runtimeDir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			return result, errors.New("实例记录不能是符号链接")
		}
		data, e := readTestInput(path)
		if e != nil {
			return result, e
		}
		var rec runtimeRecord
		if e = json.Unmarshal(data, &rec); e != nil || rec.ID != id {
			return result, errors.New("实例记录无效")
		}
		if rec.Config != cfg {
			continue
		}
		response, callErr := runtimeControlCall(ctx, runtimeDir, rec, controlRequest{Operation: "identity"})
		e = callErr
		if e != nil {
			if runtimeEndpointAbsent(e) {
				continue
			}
			return result, fmt.Errorf("无法确认运行实例：%w", e)
		}
		if rec.Directory != directory {
			return result, errors.New("实例工作目录不同，请显式指定 working-directory")
		}
		if rec.ConfigHash != hashRuntimeBytes(raw) {
			return result, errors.New("总配置已更改，需要重启实例后再刷新")
		}
		files, readErr := readLocalHooks(o)
		if readErr != nil {
			return result, readErr
		}
		cliPaths := make([]string, 0, len(files))
		for _, f := range files {
			cliPaths = append(cliPaths, f.Path)
		}
		sort.Strings(cliPaths)
		sort.Strings(response.Files)
		if !equalRuntimePaths(cliPaths, response.Files) {
			return result, errors.New("运行实例与 CLI 的有效 Hook 来源不一致")
		}
		if result.record != nil {
			return result, errors.New("存在多个匹配实例，请使用 --instance 指定")
		}
		copy := rec
		result.record = &copy
	}
	if instance != "" && result.record == nil {
		return result, errors.New("未找到指定实例")
	}
	return result, nil
}
func validRuntimeID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}

func requestRuntimeReload(ctx context.Context, selection runtimeSelection, files []localHookFile, expected map[string]string) (controlResponse, error) {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return runtimeControlCall(ctx, selection.dir, *selection.record, controlRequest{Operation: "reload", Files: paths, Expected: expected})
}

func runHooksReloadCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webhook hooks reload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setCommandHelp(fs, "重载匹配运行实例的全部 Hook 来源，并等待加载回执。")
	var o localHookOptions
	var dir, id string
	fs.StringVar(&o.Config, "config", "", "总配置文件（必填）")
	fs.StringVar(&o.Directory, "working-directory", "", "服务的实际工作目录")
	fs.StringVar(&dir, "runtime-dir", "", "显式选择运行目录")
	fs.StringVar(&id, "instance", "", "多个匹配实例时指定 ID")
	fs.BoolVar(&o.JSON, "json", false, "输出 JSON")
	timeout := fs.Duration("reload-timeout", 5*time.Second, "等待实例及重载回执的超时")
	if e := fs.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if o.Config == "" || fs.NArg() != 0 || *timeout <= 0 {
		fmt.Fprintln(stderr, "需要 config，且超时必须为正")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	selection, e := selectRuntime(ctx, o, dir, id)
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	if selection.record == nil {
		fmt.Fprintln(stderr, "未发现可刷新的运行实例")
		return 1
	}
	files, e := readLocalHooks(o)
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 1
	}
	response, e := requestRuntimeReload(ctx, selection, files, nil)
	if e != nil {
		fmt.Fprintln(stderr, "刷新未确认：", e)
		return 1
	}
	return writeManagementJSON(stdout, stderr, map[string]interface{}{"runtimeConfirmed": true, "instance": response.Instance.ID, "generation": response.Generation})
}

func runtimeSourcePaths() ([]string, error) {
	paths := append([]string(nil), configuredHookFiles...)
	for _, dir := range hooksDirectories {
		found, err := directoryHooksFiles(dir)
		if err != nil {
			return nil, errors.New("Hook 目录不可读")
		}
		for _, p := range found {
			paths = append(paths, p)
		}
	}
	unique := map[string]bool{}
	for _, p := range paths {
		unique[p] = true
	}
	paths = paths[:0]
	for p := range unique {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	return paths, nil
}
