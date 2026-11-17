package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"github.com/xtulnx/webhook/internal/hook"
)

type testPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type testUpload struct {
	Field       string `json:"field"`
	Path        string `json:"path"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content-type,omitempty"`
}
type testContext struct {
	RemoteIP    string       `json:"remote-ip"`
	Method      string       `json:"method"`
	Headers     []testPair   `json:"headers"`
	Query       []testPair   `json:"query"`
	BodyFile    string       `json:"body-file"`
	ContentType string       `json:"content-type"`
	PayloadFile string       `json:"payload-file"`
	Uploads     []testUpload `json:"uploaded-files"`
}
type testResult struct {
	Mode            string       `json:"mode"`
	Hook            string       `json:"hook"`
	Assumptions     []string     `json:"assumptions"`
	WouldTrigger    bool         `json:"wouldTrigger"`
	Executed        bool         `json:"executed"`
	Directory       string       `json:"workingDirectory,omitempty"`
	Arguments       []string     `json:"arguments,omitempty"`
	Environment     []string     `json:"environment,omitempty"`
	Files           []testUpload `json:"files,omitempty"`
	Output          string       `json:"output,omitempty"`
	CommandExitCode *int         `json:"commandExitCode,omitempty"`
	TimedOut        bool         `json:"timedOut"`
	Diagnostic      string       `json:"diagnostic,omitempty"`
}

const testInputLimit = 10 << 20

func runHookTestCommand(action string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("webhook hooks "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	setCommandHelp(fs, "直接指定本地上下文；缺少的触发条件默认免除。explain 永远不执行命令。")
	var o localHookOptions
	var c testContext
	var headers, query, uploads agentValues
	fs.StringVar(&o.Config, "config", "", "总配置文件，与 file 互斥")
	fs.StringVar(&o.File, "file", "", "Hook 文件，与 config 互斥")
	fs.StringVar(&o.Directory, "working-directory", "", "配置中相对路径的基准目录")
	fs.StringVar(&o.ID, "id", "", "Hook ID（必填）")
	fs.BoolVar(&o.Template, "template", false, "将 Hook 文件作为模板读取")
	fs.BoolVar(&o.JSON, "json", false, "输出 JSON 结果")
	fs.StringVar(&c.RemoteIP, "remote-ip", "", "直接指定有效远程 IP")
	fs.StringVar(&c.Method, "method", "", "指定请求方法")
	fs.StringVar(&c.BodyFile, "body-file", "", "原始请求体文件，与 payload-file 互斥")
	fs.StringVar(&c.PayloadFile, "payload-file", "", "已解析 payload 的 JSON 文件")
	fs.StringVar(&c.ContentType, "content-type", "", "原始请求体的 Content-Type")
	fs.Var(&headers, "header", "请求头，名称=值，可重复指定")
	fs.Var(&query, "query", "Query，名称=值，可重复指定")
	fs.Var(&uploads, "uploaded-file", "已上传文件，字段名=本地路径，可重复指定")
	fixture := fs.String("context", "", "JSON/YAML 上下文文件，与上下文参数互斥")
	strict := fs.Bool("strict", false, "禁用缺失条件免除")
	execute := fs.Bool("execute", false, "真实执行选定 Hook，仅 test 可用")
	reveal := fs.Bool("show-values", false, "显示请求派生参数值")
	timeout := fs.Duration("timeout", 30*time.Second, "本次命令执行上限，必须为正时长")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || o.ID == "" || (o.Config != "" && o.File != "") || *timeout <= 0 || (action == "explain" && *execute) {
		fmt.Fprintln(stderr, "无效参数：需要 id，config/file 互斥，explain 不接受 execute")
		return 2
	}
	base, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	contextBase := base
	if *fixture != "" {
		conflict := false
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "remote-ip", "method", "header", "query", "body-file", "payload-file", "content-type", "uploaded-file":
				conflict = true
			}
		})
		if conflict {
			fmt.Fprintln(stderr, "context 与逐项上下文参数互斥")
			return 2
		}
		raw, e := readTestInput(*fixture)
		if e != nil {
			fmt.Fprintln(stderr, e)
			return 1
		}
		raw, e = yaml.YAMLToJSON(raw)
		if e != nil {
			fmt.Fprintln(stderr, "上下文格式无效")
			return 1
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if e = dec.Decode(&c); e != nil {
			fmt.Fprintln(stderr, "上下文字段无效")
			return 1
		}
		full, _ := filepath.Abs(*fixture)
		contextBase = filepath.Dir(full)
	} else {
		for _, spec := range headers {
			k, v, ok := strings.Cut(spec, "=")
			if !ok || k == "" {
				fmt.Fprintln(stderr, "header 需要 名称=值")
				return 2
			}
			c.Headers = append(c.Headers, testPair{k, v})
		}
		for _, spec := range query {
			k, v, ok := strings.Cut(spec, "=")
			if !ok || k == "" {
				fmt.Fprintln(stderr, "query 需要 名称=值")
				return 2
			}
			c.Query = append(c.Query, testPair{k, v})
		}
		for _, spec := range uploads {
			k, v, ok := strings.Cut(spec, "=")
			if !ok || k == "" || v == "" {
				fmt.Fprintln(stderr, "uploaded-file 需要 字段名=路径")
				return 2
			}
			c.Uploads = append(c.Uploads, testUpload{Field: k, Path: v})
		}
	}
	if c.BodyFile != "" && c.PayloadFile != "" {
		fmt.Fprintln(stderr, "body-file 与 payload-file 互斥")
		return 2
	}
	if c.RemoteIP != "" && net.ParseIP(c.RemoteIP) == nil {
		fmt.Fprintln(stderr, "remote-ip 无效")
		return 2
	}
	if c.Method != "" && !validTestMethod(c.Method) {
		fmt.Fprintln(stderr, "method 无效")
		return 2
	}
	files, err := readLocalHooks(o)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var selected *hook.Hook
	for _, f := range files {
		for _, h := range f.Hooks {
			if h.ID == o.ID {
				copy := h
				selected = &copy
			}
		}
	}
	if selected == nil {
		fmt.Fprintln(stderr, "未找到 Hook:", o.ID)
		return 1
	}
	if o.Directory != "" {
		base, err = filepath.Abs(o.Directory)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	settings := map[string]interface{}{}
	if o.Config != "" {
		raw, e := os.ReadFile(o.Config)
		if e != nil {
			fmt.Fprintln(stderr, e)
			return 1
		}
		if e = yaml.Unmarshal(raw, &settings); e != nil {
			fmt.Fprintln(stderr, "总配置无效")
			return 1
		}
	}
	result := testResult{Mode: "assisted", Hook: o.ID, Assumptions: []string{}}
	if *strict {
		result.Mode = "strict"
	}
	fail := func(code int, message string) int {
		result.Diagnostic = message
		if writeManagementJSON(stdout, stderr, result) != 0 {
			return 1
		}
		return code
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(contextBase, p)
	}
	req := &hook.Request{ID: "cli-test", RealIP: c.RemoteIP, Headers: map[string]interface{}{}, Query: map[string]interface{}{}, RawRequest: &http.Request{Method: c.Method, RemoteAddr: c.RemoteIP, Header: http.Header{}, URL: &url.URL{}}}
	req.AllowSignatureErrors = selected.TriggerSignatureSoftFailures
	for _, p := range c.Headers {
		if !validTestMethod(p.Name) || strings.ContainsAny(p.Value, "\r\n") {
			return fail(1, "无效请求头")
		}
		name := textproto.CanonicalMIMEHeaderKey(p.Name)
		if _, ok := req.Headers[name]; !ok {
			req.Headers[name] = p.Value
		}
		req.RawRequest.Header.Add(name, p.Value)
	}
	for _, p := range c.Query {
		if _, ok := req.Query[p.Name]; !ok {
			req.Query[p.Name] = p.Value
		}
	}
	req.ContentType = c.ContentType
	if req.ContentType == "" {
		req.ContentType = req.RawRequest.Header.Get("Content-Type")
	}
	if selected.IncomingPayloadContentType != "" {
		req.ContentType = selected.IncomingPayloadContentType
	}
	bodyLimit := int64(testInputLimit)
	if value, ok := settings["max-body-size"]; ok {
		switch n := value.(type) {
		case float64:
			if n < 0 || n != float64(int64(n)) {
				return fail(1, "max-body-size 无效")
			}
			bodyLimit = int64(n)
		default:
			return fail(1, "max-body-size 无效")
		}
	}
	if c.BodyFile != "" {
		req.Body, err = readTestInput(resolve(c.BodyFile))
		if err != nil {
			return fail(1, "无法读取请求体")
		}
		if bodyLimit > 0 && int64(len(req.Body)) > bodyLimit {
			return fail(1, "请求体超过配置上限")
		}
		switch {
		case strings.Contains(req.ContentType, "json"):
			err = req.ParseJSONPayload()
		case strings.Contains(req.ContentType, "xml"):
			err = req.ParseXMLPayload()
		case strings.Contains(req.ContentType, "x-www-form-urlencoded"):
			err = req.ParseFormPayload()
		}
		if err != nil {
			return fail(1, "请求体解析失败")
		}
	}
	if c.PayloadFile != "" {
		raw, e := readTestInput(resolve(c.PayloadFile))
		if e != nil {
			return fail(1, "无法读取 payload")
		}
		parsed := &hook.Request{Body: raw}
		if e = parsed.ParseJSONPayload(); e != nil {
			return fail(1, "payload JSON 无效")
		}
		req.Payload = parsed.Payload
	}
	seenUploadJSON := map[string]bool{}
	for i := range c.Uploads {
		u := &c.Uploads[i]
		if u.Field == "" || strings.ContainsAny(u.Field, "/\\\x00=\r\n") {
			return fail(1, "上传字段名无效")
		}
		u.Path = resolve(u.Path)
		info, e := os.Stat(u.Path)
		if e != nil || !info.Mode().IsRegular() || info.Size() > testInputLimit {
			return fail(1, "上传文件不可读、类型不支持或超过 10 MiB")
		}
		if _, e := readTestInput(u.Path); e != nil {
			return fail(1, "上传文件不可读")
		}
		if u.Filename == "" {
			u.Filename = filepath.Base(u.Path)
		}
		parseJSON := strings.Contains(u.ContentType, "json")
		for _, j := range selected.JSONStringParameters {
			if j.Source == hook.SourcePayload && j.Name == u.Field {
				parseJSON = true
			}
		}
		firstUpload := !seenUploadJSON[u.Field]
		seenUploadJSON[u.Field] = true
		if parseJSON && firstUpload {
			if req.Payload == nil {
				req.Payload = map[string]interface{}{}
			}
			if _, exists := req.Payload[u.Field]; !exists {
				raw, e := readTestInput(u.Path)
				if e != nil {
					return fail(1, "上传文件读取失败")
				}
				var value map[string]interface{}
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.UseNumber()
				if e = dec.Decode(&value); e != nil {
					return fail(1, "上传 JSON 无效")
				}
				req.Payload[u.Field] = value
			}
		}
	}
	// Uploaded JSON is already decoded, unlike string parameters from body/query.
	filtered := selected.JSONStringParameters[:0]
	for _, j := range selected.JSONStringParameters {
		uploaded := false
		for _, u := range c.Uploads {
			if j.Source == hook.SourcePayload && j.Name == u.Field {
				uploaded = true
			}
		}
		if !uploaded {
			filtered = append(filtered, j)
		}
	}
	selected.JSONStringParameters = filtered
	if errs := selected.ParseJSONParameters(req); len(errs) > 0 {
		return fail(1, "parse-parameters-as-json 输入缺失或无效")
	}
	if err = validateTestRules(selected.TriggerRule); err != nil {
		return fail(1, err.Error())
	}
	allowed, err := testAccess(selected, settings, c, *strict, &result.Assumptions)
	if err != nil {
		return fail(1, err.Error())
	}
	if !allowed {
		return fail(4, "方法或 IP 条件拒绝")
	}
	rule, err := filterTestRules(selected.TriggerRule, req, c, *strict, &result.Assumptions, "rule")
	if err != nil {
		return fail(1, err.Error())
	}
	if rule != nil {
		allowed, e := rule.Evaluate(req)
		if e != nil && !hook.IsParameterNodeError(e) {
			return fail(1, "触发规则求值失败")
		}
		if !allowed {
			return fail(4, "触发规则拒绝")
		}
	}
	result.WouldTrigger = true
	for _, a := range append(append(append([]hook.Argument{}, selected.PassArgumentsToCommand...), selected.PassEnvironmentToCommand...), selected.PassFileToCommand...) {
		if !testArgumentPresent(a, req, c) {
			return fail(1, "命令映射缺少输入："+a.Source+"/"+a.Name)
		}
	}
	argv, errs := selected.ExtractCommandArguments(req)
	if len(errs) > 0 {
		return fail(1, "命令参数映射失败")
	}
	env, errs := selected.ExtractCommandArgumentsForEnv(req)
	if len(errs) > 0 {
		return fail(1, "环境映射失败")
	}
	for _, a := range selected.PassFileToCommand {
		v, e := a.Get(req)
		if e != nil {
			return fail(1, "文件映射失败")
		}
		if a.Base64Decode {
			if _, e := base64.StdEncoding.DecodeString(v); e != nil {
				return fail(1, "文件映射 Base64 无效")
			}
		}
	}
	if selected.CommandWorkingDirectory == "" {
		if !filepath.IsAbs(selected.ExecuteCommand) {
			candidate := selected.ExecuteCommand
			if strings.ContainsAny(candidate, "/\\") {
				candidate = filepath.Join(base, candidate)
			}
			if resolved, e := exec.LookPath(candidate); e == nil {
				selected.ExecuteCommand, _ = filepath.Abs(resolved)
			}
		}
		selected.CommandWorkingDirectory = base
	} else if !filepath.IsAbs(selected.CommandWorkingDirectory) {
		selected.CommandWorkingDirectory = filepath.Join(base, selected.CommandWorkingDirectory)
	}
	result.Directory = selected.CommandWorkingDirectory
	result.Arguments = argv
	result.Environment = env
	result.Files = append([]testUpload(nil), c.Uploads...)
	if !*reveal {
		for i := range result.Arguments {
			if i > 0 {
				result.Arguments[i] = "[HIDDEN]"
			}
		}
		for i, v := range result.Environment {
			k, _, _ := strings.Cut(v, "=")
			result.Environment[i] = k + "=[HIDDEN]"
		}
		for i := range result.Files {
			result.Files[i].Path = "[HIDDEN]"
		}
	}
	if action == "explain" || !*execute {
		return writeManagementJSON(stdout, stderr, result)
	}
	info, checkErr := os.Stat(selected.CommandWorkingDirectory)
	if checkErr != nil || !info.IsDir() {
		return fail(1, "命令工作目录不可用")
	}
	candidate := selected.ExecuteCommand
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(selected.CommandWorkingDirectory, candidate)
	}
	if _, checkErr = exec.LookPath(candidate); checkErr != nil {
		return fail(1, "命令不可执行或未找到")
	}
	// Stage copies and inject the same environment mapping used by uploaded files online.
	dir, e := os.MkdirTemp("", "webhook-test-*")
	if e != nil {
		return fail(1, "无法创建测试临时目录")
	}
	defer os.RemoveAll(dir)
	if selected.KeepFileEnvironment {
		seen := map[string]bool{}
		for i, u := range c.Uploads {
			if seen[u.Field] {
				continue
			}
			seen[u.Field] = true
			raw, e := readTestInput(u.Path)
			if e != nil {
				return fail(1, "读取上传文件失败")
			}
			path := filepath.Join(dir, fmt.Sprintf("upload-%d", i))
			if e = os.WriteFile(path, raw, 0600); e != nil {
				return fail(1, "暂存上传文件失败")
			}
			for k, v := range map[string]string{hook.EnvNamespace + "FILE_" + strings.ToUpper(u.Field): path, hook.EnvNamespace + "FILENAME_" + strings.ToUpper(u.Field): u.Filename} {
				selected.PassEnvironmentToCommand = append(selected.PassEnvironmentToCommand, hook.Argument{Source: hook.SourceString, Name: v, EnvName: k})
			}
		}
	}
	limit := *timeout
	if value, ok := settings["command-timeout"]; ok && fmt.Sprint(value) != "" {
		v := fmt.Sprint(value)
		d, e := time.ParseDuration(v)
		if e != nil || d < 0 {
			return fail(1, "全局 command-timeout 无效")
		}
		if d > 0 && d < limit {
			limit = d
		}
	}
	if d, e := selected.EffectiveCommandTimeout(0); e != nil {
		return fail(1, "command-timeout 无效")
	} else if d > 0 && d < limit {
		limit = d
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req.RawRequest = req.RawRequest.WithContext(ctx)
	result.Executed = true
	result.Output, err = executeHookWithLogger(selected, req, true, func() {}, func(string, ...interface{}) {})
	result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	exit := 0
	if err != nil {
		exit = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		}
		result.Diagnostic = "命令执行失败"
	}
	result.CommandExitCode = &exit
	if writeManagementJSON(stdout, stderr, result) != 0 {
		return 1
	}
	if err != nil {
		return 1
	}
	return 0
}

func readTestInput(path string) ([]byte, error) {
	original, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !original.Mode().IsRegular() {
		return nil, errors.New("输入必须是普通文件")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("输入必须是普通文件")
	}
	data, err := io.ReadAll(io.LimitReader(f, testInputLimit+1))
	if len(data) > testInputLimit {
		return nil, errors.New("输入超过 10 MiB")
	}
	return data, err
}
func validTestMethod(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}
func testArgumentPresent(a hook.Argument, r *hook.Request, c testContext) bool {
	switch a.Source {
	case hook.SourceRawRequestBody:
		return c.BodyFile != ""
	case hook.SourceEntirePayload:
		return r.Payload != nil
	case hook.SourceEntireQuery:
		return len(c.Query) > 0
	case hook.SourceEntireHeaders:
		return len(c.Headers) > 0
	case hook.SourceRequest:
		if a.Name == "remote-addr" {
			return c.RemoteIP != ""
		}
		if a.Name == "method" {
			return c.Method != ""
		}
	}
	_, err := a.Get(r)
	return err == nil
}
func validateTestRules(r *hook.Rules) error {
	if r == nil {
		return nil
	}
	count := 0
	if r.And != nil {
		count++
	}
	if r.Or != nil {
		count++
	}
	if r.Not != nil {
		count++
	}
	if r.Match != nil {
		count++
	}
	if count != 1 {
		return errors.New("规则必须指定一个操作类型")
	}
	if r.Match != nil {
		m := r.Match
		if m.Type != hook.IPWhitelist {
			switch m.Parameter.Source {
			case hook.SourceHeader, hook.SourceQuery, hook.SourceQueryAlias, hook.SourcePayload, hook.SourceString, hook.SourceRawRequestBody, hook.SourceEntirePayload, hook.SourceEntireQuery, hook.SourceEntireHeaders:
			case hook.SourceRequest:
				if m.Parameter.Name != "remote-addr" && m.Parameter.Name != "method" {
					return errors.New("规则 request 参数无效")
				}
			default:
				return errors.New("规则参数来源无效")
			}
		}
		switch m.Type {
		case hook.MatchRegex:
			if _, e := regexp.Compile(m.Regex); e != nil {
				return errors.New("规则正则无效")
			}
		case hook.IPWhitelist:
			if _, e := hook.CheckIPWhitelist("127.0.0.1", m.IPRange); e != nil {
				return errors.New("规则 IP 范围无效")
			}
		case hook.MatchValue, hook.MatchHMACSHA1, hook.MatchHMACSHA256, hook.MatchHMACSHA512, hook.MatchHashSHA1, hook.MatchHashSHA256, hook.MatchHashSHA512:
		case hook.ScalrSignature:
			return errors.New("本地测试暂不支持 scalr-signature")
		default:
			return errors.New("未知规则类型")
		}
	}
	if r.And != nil {
		for _, v := range *r.And {
			if e := validateTestRules(&v); e != nil {
				return e
			}
		}
	}
	if r.Or != nil {
		for _, v := range *r.Or {
			if e := validateTestRules(&v); e != nil {
				return e
			}
		}
	}
	if r.Not != nil {
		v := hook.Rules(*r.Not)
		return validateTestRules(&v)
	}
	return nil
}
func filterTestRules(r *hook.Rules, req *hook.Request, c testContext, strict bool, assumptions *[]string, path string) (*hook.Rules, error) {
	if r == nil {
		return nil, nil
	}
	if r.And != nil || r.Or != nil {
		children := []hook.Rules{}
		original := []hook.Rules{}
		if r.And != nil {
			original = []hook.Rules(*r.And)
		} else {
			original = []hook.Rules(*r.Or)
		}
		for i, v := range original {
			child, e := filterTestRules(&v, req, c, strict, assumptions, fmt.Sprintf("%s/%d", path, i))
			if e != nil {
				return nil, e
			}
			if child != nil {
				children = append(children, *child)
			}
		}
		if len(children) == 0 {
			if len(original) == 0 {
				return r, nil
			}
			return nil, nil
		}
		out := &hook.Rules{}
		if r.And != nil {
			x := hook.AndRule(children)
			out.And = &x
		} else {
			x := hook.OrRule(children)
			out.Or = &x
		}
		return out, nil
	}
	if r.Not != nil {
		v := hook.Rules(*r.Not)
		child, e := filterTestRules(&v, req, c, strict, assumptions, path+"/not")
		if child == nil || e != nil {
			return nil, e
		}
		n := hook.NotRule(*child)
		return &hook.Rules{Not: &n}, nil
	}
	if r.Match != nil {
		m := r.Match
		present := testArgumentPresent(m.Parameter, req, c)
		if m.Type == hook.IPWhitelist {
			present = c.RemoteIP != ""
		}
		if !present && !strict {
			*assumptions = append(*assumptions, path)
			return nil, nil
		}
		if present && (strings.Contains(m.Type, "hmac") || strings.Contains(m.Type, "hash")) && c.BodyFile == "" {
			return nil, errors.New("已指定签名，但缺少原始 body-file")
		}
	}
	return r, nil
}
func testAccess(h *hook.Hook, cfg map[string]interface{}, c testContext, strict bool, a *[]string) (bool, error) {
	methods := h.HTTPMethods
	if len(methods) == 0 {
		if s, ok := cfg["http-methods"].(string); ok && s != "" {
			methods = strings.Split(s, ",")
		}
	}
	if len(methods) > 0 {
		if c.Method == "" {
			if strict {
				return false, errors.New("缺少 method")
			}
			*a = append(*a, "http-methods")
		} else {
			ok := false
			for _, m := range methods {
				if c.Method == strings.ToUpper(strings.TrimSpace(m)) {
					ok = true
				}
			}
			if !ok {
				return false, nil
			}
		}
	}
	allow, _ := cfg["access-whitelist"].(string)
	deny, _ := cfg["access-blacklist"].(string)
	if allow != "" && deny != "" {
		return false, errors.New("IP 白名单与黑名单互斥")
	}
	an, e := parseIPNetworks(allow, "access-whitelist")
	if e != nil {
		return false, errors.New("IP 白名单无效")
	}
	dn, e := parseIPNetworks(deny, "access-blacklist")
	if e != nil {
		return false, errors.New("IP 黑名单无效")
	}
	if len(an)+len(dn) > 0 {
		if c.RemoteIP == "" {
			if strict {
				return false, errors.New("缺少 remote-ip")
			}
			*a = append(*a, "access-ip")
		} else {
			ip := net.ParseIP(c.RemoteIP)
			if len(an) > 0 && !containsIP(an, ip) || containsIP(dn, ip) {
				return false, nil
			}
		}
	}
	return true, nil
}
