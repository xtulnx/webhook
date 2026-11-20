package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/xtulnx/webhook/internal/network"
)

type agentValues []string

func (v *agentValues) String() string     { return strings.Join(*v, ",") }
func (v *agentValues) Set(s string) error { *v = append(*v, s); return nil }

func runAgentCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("webhook agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	setCommandHelp(flags, "使用 Hub 预先授权的节点标识和令牌临时加入组网，可选发布服务或建立本机转发。\n"+
		"文件分享地址格式为 下载基址/节点ID[/链接ID]/共享名[/目录内路径]，共享名必填。\n"+
		"仅发布临时文件分享时，同一节点 ID 可并行注册。各进程使用不同的非空链接 ID，或由 Agent 自动生成。")
	cfg := &network.AgentConfig{}
	flags.StringVar(&cfg.Server, "server", "", "Hub 基础地址，需包含自定义反向代理路径前缀（如有）")
	flags.StringVar(&cfg.TransportPrefix, "transport-prefix", "", "Hub 配置的组网前缀，默认 /__webhook_network/v1")
	flags.StringVar(&cfg.ID, "id", "", "Hub 预先授权的节点标识")
	flags.StringVar(&cfg.Token, "token", "", "直接指定节点令牌（会出现在进程参数中，建议使用 token-env 或 token-file）")
	flags.StringVar(&cfg.TokenEnv, "token-env", "", "保存节点令牌的环境变量名称")
	flags.StringVar(&cfg.TokenFile, "token-file", "", "保存节点令牌的文件路径")
	flags.StringVar(&cfg.CAFile, "ca-file", "", "私有 CA 证书路径（可选）")
	flags.IntVar(&cfg.MaxConnections, "max-connections", 8, "最大并发隧道数")
	lifetime := flags.Duration("duration", time.Hour, "到达指定时长后退出，必须大于 0（例如 30m、1h）")
	connect := flags.String("connect", "", "通过标准输入输出连接已授权的 节点/服务，不监听本地端口")
	shareLink := flags.Bool("share-link", false, "为每个 --share 输出临时下载链接，需要 Hub 为本节点启用 allow_share_links")
	var shareLinkID *string
	flags.Func("share-link-id", "自定义本次分享的 `链接ID`，须配合 --share-link。非空值仅允许英文字母、数字、下划线和连字符。\n"+
		"省略参数时自动生成。传空字符串会省略路径段，并独占该节点的公网分享路径。", func(value string) error {
		shareLinkID = &value
		return nil
	})
	shareIndex := flags.Bool("share-index", false, "在各共享目录下提供只读索引，并优先预览支持的文本、PDF 和图片")
	shareUser := flags.String("share-basic-user", "", "临时文件分享的 Basic Auth 用户名")
	sharePassword := flags.String("share-basic-password", "", "临时文件分享的 Basic Auth 密码（会出现在进程参数中）")
	shareAuthFile := flags.String("share-basic-auth-file", "", "从本地文件读取一行 Basic Auth 凭证，格式为 用户名:密码")
	var shares, publishes, forwards agentValues
	flags.Var(&shares, "share", "共享只读文件或目录，格式为 URL名称=本地路径，可重复指定；服务名为 files")
	flags.Var(&publishes, "publish", "发布 TCP 服务，格式为 服务名=主机:端口，可重复指定")
	flags.Var(&forwards, "forward", "本机回环转发入口，格式为 主机:端口=节点/服务，可重复指定")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "error:", err); return 2 }
	if flags.NArg() != 0 || *lifetime <= 0 {
		return fail(errors.New("unexpected arguments or invalid duration"))
	}
	if len(shares) == 0 && (*shareLink || *shareIndex || *shareUser != "" || *sharePassword != "" || *shareAuthFile != "") {
		return fail(errors.New("share options require at least one --share"))
	}
	if shareLinkID != nil && (!*shareLink || (*shareLinkID != "" && !network.ValidShareName(*shareLinkID))) {
		return fail(errors.New("share-link-id requires --share-link and must be empty or a single segment containing only ASCII letters, digits, underscores, and hyphens"))
	}
	if *connect != "" && len(shares)+len(publishes)+len(forwards) != 0 {
		return fail(errors.New("connect cannot be combined with share, publish, or forward"))
	}
	handlers := map[string]http.Handler{}
	var shareAuth *shareBasicAuth
	if len(shares) > 0 {
		var err error
		shareAuth, err = loadShareBasicAuth(*shareUser, *sharePassword, *shareAuthFile)
		if err != nil {
			return fail(err)
		}
		handler, err := newShareHandler(shares, *shareIndex, shareAuth)
		if err != nil {
			return fail(err)
		}
		handlers["files"] = handler
		cfg.Services = append(cfg.Services, network.Service{Name: "files", InProcess: true})
	}
	for _, value := range publishes {
		name, target, ok := strings.Cut(value, "=")
		if !ok || name == "webhook" {
			return fail(errors.New("publish requires name=host:port; webhook is reserved"))
		}
		cfg.Services = append(cfg.Services, network.Service{Name: name, Target: target})
	}
	for _, value := range forwards {
		listen, to, ok := strings.Cut(value, "=")
		if !ok {
			return fail(errors.New("forward requires host:port=node/service"))
		}
		cfg.Forwards = append(cfg.Forwards, network.Forward{Listen: listen, To: to})
	}
	if err := network.ValidateAgent(cfg); err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *lifetime)
	defer cancel()
	if *connect != "" {
		if err := network.Connect(ctx, cfg, *connect, os.Stdin, stdout); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	options := network.AgentOptions{Handlers: handlers, Version: version, Temporary: true, ExpiresAt: agentExpiry(ctx)}
	if *shareLink {
		options.ShareBasic = shareAuth != nil
		options.ShareLinkID = shareLinkID
		for _, value := range shares {
			name, _, _ := strings.Cut(value, "=")
			options.ShareNames = append(options.ShareNames, name)
		}
		options.OnShareReady = func(links []string) {
			for _, link := range links {
				fmt.Fprintln(stdout, link)
			}
		}
	}
	fmt.Fprintf(stdout, "temporary agent %s; lifetime %s; hub authorization required\n", cfg.ID, *lifetime)
	if err := network.RunAgent(ctx, cfg, options); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func agentExpiry(ctx context.Context) *time.Time {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	deadline = deadline.UTC()
	return &deadline
}
