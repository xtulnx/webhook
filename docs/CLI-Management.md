# CLI 本地 Hook 管理

目前已实现 `list`、`show`、`validate`、`add/update/delete` 与主动 `reload`、`config init`，以及下文的本地 `test` / `explain`。查询与校验命令使用同一个 webhook 可执行文件，不启动 HTTP 服务、watcher、Admin 或组网连接，不执行 Hook 命令。

```sh
webhook hooks list --config ./webhook.yaml
webhook hooks list --file ./hooks.yaml --json
webhook hooks show --file ./hooks.yaml --id deploy
webhook hooks validate --config ./webhook.yaml
webhook hooks validate --file ./hooks.yaml --template
```

`--config` 与 `--file` 互斥；未选择来源时读取当前工作目录的 `hooks.json`。总配置里的 `hooks` / `hooks-dir` 来源按现有规则读取，目录不递归，目录外符号链接按原目录扫描规则忽略。来源路径归一化、去重，跨文件 ID 必须唯一。没有本地 Hook 的纯 Hub/Agent 配置可得到空集合。

总配置路径本身相对当前目录；配置内 Hook 相对路径默认也以当前目录为基准。用 `--working-directory` 显式指定配置中的相对路径基准，命令不会改变进程工作目录。例如：

```sh
webhook hooks validate --config /srv/webhook/webhook.yaml --working-directory /srv/webhook
```

`show` 要求 `--id`。默认递归隐藏已知 secret/token/password 字段，并附带敏感数据提示；其他字段仍可能含敏感内容。`--show-secrets` 显示完整定义，请按需要使用。列表仅返回 ID 与来源路径。

管理命令严格拒绝未知 Hook 字段、重复 YAML 键、空或重复 ID，以及非法执行超时/并发限制。任何选定来源读取或解析失败均返回非零，多个来源错误汇总输出，不把部分加载成功显示为整体校验成功。为避免配置值或展开的密钥泄露，Hook 解析错误只报告文件及失败类别。

模板使用与服务相同的解析函数，可读取显式引用的环境变量或文件。配置通过仅表示当前用户环境下的解析与结构检查成功，不代表脚本存在、触发规则一定满足或实际执行必定成功。普通服务加载保留原有兼容行为，因此管理校验可能拒绝服务忽略的未知字段。

结果可用 `--json` 输出。stdout 为成功结果，stderr 为诊断；退出码 0 表示成功、1 表示读取/校验/查询失败、2 表示用法错误。查询的是磁盘配置，不代表运行实例已加载的配置。

network 管理和 Windows service 管理仍在[开发计划](CLI-Management-Plan.md)中，当前尚不可用。

## 增删改与生效确认

```sh
webhook hooks add --config ./webhook.yaml --file ./hooks.yaml --from ./new-hook.json --dry-run
webhook hooks add --config ./webhook.yaml --file ./hooks.yaml --from ./new-hook.json
webhook hooks update --config ./webhook.yaml --file ./hooks.yaml --id deploy --from ./deploy.json
webhook hooks delete --config ./webhook.yaml --file ./hooks.yaml --id deploy
```

总配置与目标文件必须显式指定，目标必须属于该配置的已存在 JSON/YAML Hook 来源；目标相对路径以 `--working-directory` 或当前目录为准。`--from` 相对当前目录，内容是一个完整 Hook 对象，不能是数组。add 不接受已有 ID，update 要求原 ID 存在且保持不变，delete 要求 ID 存在。首版不创建 Hook 文件，不自动修改总配置；模板模式、未知字段、非普通文件/符号链接写入均拒绝。

所有候选来源重新读取并检查跨文件 ID，任何来源无效都拒绝保存。实际写入使用同一套目录级操作系统文件锁，与新版 Admin 协调；持锁后读取最新磁盘内容。遇锁冲突立即返回失败，可稍后重试。目录内保留 `.webhook-hooks.lock`，请不要在服务运行时删除锁文件。

保存前创建 `<原文件名>.backup-<随机后缀>`，保留原始字节；备份与锁文件不会被 hooks-dir 扫描。输出 backup 为实际备份路径。保存将重新序列化目标文件，注释和排版可能丢失。备份由操作者按需清理，不自动覆盖或删除旧备份。

文件替换失败不采用“先删除原文件”的回退。Unix 保留所有者、组与 POSIX 权限位；Windows 使用原生文件替换和显式安全描述符处理，不能保留所需元数据时失败。Windows 当前已完成构建验证，实际权限和文件锁行为仍需在 Windows 验收。普通编辑器与旧版程序不遵循协作锁，提交前比较可检测已知变化，但不能承诺对任意外部编辑器提供文件系统级比较交换。

`--dry-run` 只输出操作类型、目标、ID 和前后数量，不创建锁/备份、不改文件；不输出可能含敏感值的完整配置。其结果是当时的快照，真实写入会重新检查。

保存后默认 `--reload auto`：在配置指定的私有运行目录中查找匹配实例，找到则请求整个 Hook 集合重载并等待回执。没有发现实例时仍保存，结果显示 `reload: not-found`、`runtimeConfirmed: false`。`--reload required` 要求保存前确认实例身份，保存后确认加载；前置检查失败不写文件。`--reload never` 只保存，不主动请求，已有 hotreload 仍可能生效。

保存后的刷新失败或超时返回 **3**，JSON 保留 `saved: true`、`runtimeConfirmed: false` 和备份路径，不自动回滚文件。此时用 `hooks reload` 重试，而不是重复 add/delete。拿到回执只表示回执当时目标内容已加载，不表示后续不会再被其他编辑更新，也不取消在途命令。

## 分类模板：查看、填写、校验、应用

交互向导已移除，改用内置模板；不再支持 `hooks add --interactive/--apply`。所有模板编译在同一个 webhook 程序中，复制单个程序即可生成，不需要网络、脚本或附带资源目录。

```sh
webhook config template list
webhook config template global                       # 在终端查看带中文注释的 YAML
webhook config template hook --output new-hook.yaml  # 新建单个 Hook 对象
webhook config template hooks --output hooks.json    # 根据扩展名自动生成 JSON 数组
webhook config template hub                         # 仅 network 字段，合入已有总配置
webhook config template global hub admin --output hub.yaml # 一次组合总配置
webhook config template global agent tls --output agent.json # JSON 无注释
```

| 模板名称 | 顶层结构及用途 | 填写要求 |
| --- | --- | --- |
| global | 完整服务对象，交给 `webhook --config` | 准备 hooks 数组文件；注释按监听、路径、执行、日志及扩展功能分组 |
| hook | 单个 Hook 对象，交给 `hooks add/update --from` | 必改命令路径和 ID；说明参数/环境/文件映射、触发规则及响应行为 |
| hooks | Hook 对象数组，交给 `-hooks` 或全局 `hooks` | 必改示例命令路径；不可把整个数组传给 add --from |
| hub / agent / both | 仅包含 network 的总配置片段；可与 global/admin/tls 组合 | 启动前准备各自 token_env 指定的高熵令牌和真实地址；both 为级联示例 |
| network-hub / network-agent / network-both | 独立组网对象，交给全局 `network-config` | 不能直接当作 `--config` 总配置；支持节点、路由、服务、转发和级联字段说明 |
| admin | 仅 Admin、TOTP/JWT 与更新检查字段 | 默认关闭 Admin；替换占位密钥后再启用 |
| tls | 仅 HTTPS 字段，不改变监听端口 | 准备真实证书、私钥；在总配置设置 port（例如 9443） |
| context | 本地 test/explain 上下文 | 可选 body/payload/上传文件；引用文件路径相对上下文文件目录 |

**组合规则：** 模块名以空格分隔，放在选项之前，例如 `global hub admin --format yaml`。只有 `global`、`hub/agent/both`、`admin`、`tls` 共用总配置结构，可自由组合；无需强制包含 global，例如 `hub admin` 可以生成用于手工合入现有配置的片段。输出始终按“global → 组网 → admin → tls”排序，保留注释，生成单个对象，不依赖参数顺序覆盖字段。重复模块、重复配置键或多种组网模式会报错；级联使用 `both`，不拼接 `hub agent`。

`hook`、`hooks`、`context`、`network-*` 的文件结构或加载入口不同，必须各自生成。子模块在 YAML 和 JSON 下都只输出目标字段，不再附带全局配置；原先依赖 `template hub/agent/both/admin/tls` 生成完整文件的用法，改为在模块前加 `global`。片段不会继承 global 模板中的本机监听等默认示例，不建议直接作为完整部署配置使用。

`config init --profile hub|agent|both` 仍生成含全局设置和空 Hook 文件的完整目录。模板组合不擅自修改 global 的端口；级联示例需手动区分上下游端口（init 的 both 场景仍默认 9001）。模板只生成新内容，合入已有配置仍由用户编辑完成。

YAML 的常用字段直接启用，其他功能以注释示例提供；注释指出默认值、相对路径基准、互斥配置和风险边界。取消注释时按场景选择，不要把互斥片段和重复键一起启用。JSON 只保留对应的有效配置字段；要查完整功能和可选字段说明，可查看同名 YAML 模板。目录支持 `list --json`，每项含用途与使用入口。

`--output` 不存在时只向 stdout 输出配置，可自行重定向；使用 `--output` 则以 0600 权限新建文件，拒绝覆盖已有文件或符号链接，不隐式创建父目录。`.json` / `.yaml` / `.yml` 自动决定格式；与显式 `--format` 冲突时拒绝。生成过程不自动改现有配置、不重载、不执行命令。

典型操作：

```sh
webhook config init --dir ./local
webhook config template hook --output ./local/new-hook.yaml
# 用编辑器按注释填写 ID、程序路径及所需参数/规则；示例不会携带可用发布命令或固定密钥。
webhook hooks add --config ./local/webhook.yaml --file ./local/hooks.yaml --from ./local/new-hook.yaml --dry-run
webhook hooks add --config ./local/webhook.yaml --file ./local/hooks.yaml --from ./local/new-hook.yaml
webhook hooks validate --config ./local/webhook.yaml
```

生成全局/组网模板的目的为编辑和学习，不代表相关配置热更新已实现。Windows 服务和网络配置在线管理仍属于未完成的后续能力；模板注释不会把规划项描述为可用功能。

## 实例发现与主动刷新

总配置加入可选运行目录；相对路径仍基于进程工作目录。未配置时保留原运行方式，不创建控制通道。

```yaml
runtime-dir: /var/lib/webhook/runtime
```

```sh
webhook hooks reload --config ./webhook.yaml
webhook hooks update --config ./webhook.yaml --file ./hooks.yaml --id deploy --from ./deploy.json --reload required
```

运行目录的父目录需已存在。Unix 要求当前服务用户独占的 0700 普通目录，程序可创建末级目录；Windows 使用受保护的显式 DACL，只允许所有者和系统管理员等受信主体。通过 `--runtime-dir` 可显式选择控制目录，多个匹配实例用 `--instance` 选择，`--reload-timeout` 默认 5 秒。

程序记录随机实例 ID、配置路径、工作目录及启动配置摘要。CLI 核对身份、工作目录与有效 Hook 文件集合，不扫描所有进程或端口；启动时 CLI 额外追加了来源而本次管理命令未选择它们时会拒绝，避免刷新错误集合。崩溃残留记录通过连接验证判废，不按 PID 发信号；正常退出清理自身记录和端点。总配置文件内容修改后要求重启，不将其作为 hooks reload 的一部分生效。

Unix 使用私有 Unix socket；路径较长时，端点放入 /tmp 下按实例派生的 0700 私有短目录，身份记录仍在 runtime-dir，正常退出一并清理。Windows 使用受限 named pipe，均在原进程处理，不新增 TCP 管理端口或额外程序，不要求启用 Admin。端点只提供身份查询和 hooks reload，不能指定任意读写路径、运行命令或重启程序。Windows 已构建验证，ACL、服务账号和管道行为仍待实机验收。

主动 reload 严格检查全部候选来源并一次发布；任何错误保留刷新开始时的运行快照。watcher 与 Admin 的读取/发布进行串行协调，避免旧读取覆盖新发布。现有 watcher 可能早于主动请求加载，刷新失败不表示此前从未生效。原有 Unix 信号和 watcher 的触发范围继续保留。仅通过总配置关联的常驻 Hook 实例支持本阶段 CLI 自动发现；无总配置的前台进程及纯组网 Agent 的控制接入尚待后续扩展。

## 最小配置初始化

```sh
webhook config init --dir ./webhook-local --dry-run
webhook config init --dir ./hub-local --profile hub
webhook config init --dir ./agent-local --profile agent --format json
webhook config init --dir ./webhook-local
webhook --config ./webhook-local/webhook.yaml
```

在新目录或已有空目录创建 webhook.yaml、空 hooks.yaml 和 README.txt，拒绝覆盖。支持 global（默认）、hub、agent、both 场景和 yaml/json 格式，YAML 内含中文字段注释，JSON 无注释。默认只监听 127.0.0.1:9000（级联模板为 9001），Admin 关闭，启用 Hook 文件监听和私有运行目录，nopanic 允许空 Hook 启动；此时 /status 的 degraded 是尚未添加 Hook 的预期结果。配置引用绝对路径，搬迁后需要更新。初始化不启动、不注册系统服务、不下载额外组件；预览不创建任何目录或文件。

## 本地 test / explain

直接指定已接收的上下文，不发 HTTP 请求，也不要求服务已运行：

```sh
webhook hooks explain --file ./hooks.yaml --id deploy --query env=staging
webhook hooks test --file ./hooks.yaml --id deploy --remote-ip 192.0.2.10 --query env=staging --uploaded-file artifact=./package.zip --execute
```

默认辅助模式：缺少的触发条件记为 `assumptions`，允许继续；明确提供的错误值照常拒绝。缺少 IP 不会自动选取白名单地址。缺失 OR 分支不能掩盖已提供分支的失败，NOT 下的缺失条件整项免除。`--strict` 关闭免除。命令映射需要的数据必须提供；缺少 argv/env/文件来源时在执行前失败。

`explain` 永远不执行；`test` 仅在指定 `--execute` 时执行一次并同步等待。使用 CLI 当前账号和环境，共用原有执行器，默认最多 30 秒；`--timeout` 可调整，与非零 Hook/全局超时取更短值。Ctrl+C 取消直接子进程，不承诺取消全部派生进程。命令会真实产生业务副作用。

支持重复 `--header 名称=值`、`--query 名称=值`、`--uploaded-file 字段名=路径`、`--method`、`--body-file` / `--payload-file`、`--content-type`。body-file 与 payload-file 互斥。原始 body 复用 JSON/XML/URL 编码表单解析；payload-file 是已解析 JSON。指定签名时必须有原始 body-file，不能由 payload 推测签名字节。Scalr 签名的本地预演尚不支持，会明确报错。

文件作为已经上传的记录参与处理，不编码 multipart。JSON 上传可由 Content-Type 或 parse-parameters-as-json 指定；keep-file-environment 沿用线上 HOOK_FILE_/HOOK_FILENAME_ 语义，同名文件使用第一项。执行时提供临时副本并清理，不删除上传源文件。输入文件仅支持普通文件，每份最多 10 MiB；原始 body 还受总配置 max-body-size 限制。

可通过 `--context` 指定 JSON/YAML，文件内路径相对上下文文件目录。它与逐项上下文参数互斥：

```yaml
remote-ip: 192.0.2.10
method: POST
query:
  - name: env
    value: staging
headers:
  - name: Content-Type
    value: application/json
body-file: request.json
uploaded-files:
  - field: artifact
    path: package.zip
    filename: release.zip
    content-type: application/zip
```

结果始终输出 JSON（兼容接受 `--json`），包含 wouldTrigger、assumptions、executed、commandExitCode、timedOut 与诊断。请求派生参数值默认隐藏，`--show-values` 展开；脚本输出单独放在 output，可能含脚本自行打印的敏感信息。退出码 0 表示可执行或命令成功，1 表示输入/映射/执行失败，2 表示用法错误，4 表示已评估条件拒绝。test 不加 execute 与 explain 均只解释，不代表脚本已运行。

这两项只验证本地功能，不验证线上服务环境、代理信任链、TLS、Hub 路由或服务进程的并发状态；生产规则的默认行为保持不变。

## Linux systemd 服务管理

Linux 代码已接入原生 D-Bus 接口，所有操作由同一个 webhook 完成，无配套脚本、systemctl 或第三方 wrapper。当前开发环境为 macOS，已完成策略测试与 Linux 构建，尚需真实 Linux systemd 环境验收；不要将构建成功等同于服务安装成功。本轮没有在任何机器安装服务。

在具备系统级管理权限的账号下操作；安装前准备已存在的服务用户、工作目录、可读配置与 Hook 文件，以及服务用户有权限执行的业务命令：

```sh
webhook service install --config /etc/webhook/webhook.yaml --working-directory /var/lib/webhook --user webhook --dry-run
webhook service install --config /etc/webhook/webhook.yaml --working-directory /var/lib/webhook --user webhook
webhook service start
webhook service status --json
webhook service restart
webhook service enable
webhook service disable
webhook service stop
webhook service uninstall
```

固定管理 `/etc/systemd/system/webhook.service`。安装预览不写文件、不连接 systemd；实际安装通过独占创建与持久锁协调，不覆盖现有单元。相同且未经修改的受管单元可幂等成功。新安装默认不启动、不设置开机自启；start/stop 与 enable/disable 分别控制当前运行状态和开机设置。

单元内记录程序、总配置、工作目录和服务用户，全部路径绝对化并处理 systemd 的参数、变量及 specifier 展开。需要移动安装位置或改变单元设置时，先停止并卸载原受管单元，再重新安装；不提供隐式覆盖。程序二进制无需复制到其他位置，服务直接调用安装命令所使用的同一程序。

默认使用 systemd 创建的 `/run/webhook` 私有运行目录；总配置可指定已有父目录下的自定义持久运行目录。自定义目录不存在时仅创建该状态目录并赋予服务用户所有权；已有目录所有者/权限不匹配时拒绝，不改业务目录权限。`/run` 下首版只支持 `/run/webhook`。CLI 可从对应受管单元记录发现实际运行目录，不需额外安装记录脚本。

安装检查普通 POSIX 权限，不执行用户命令或临时切换身份；复杂 ACL 额外授权可能被保守拒绝。init 生成的 0600 配置和 0700 目录若由其他账号创建，需由运维预先为指定服务用户准备访问权限。启动仍可能因该服务用户的 PATH、模板环境、证书或业务权限而失败。

仅管理 root 拥有且其他用户不可写的原始受管单元，检测到内容修改、其他加载路径、drop-in 或 socket 触发关联时拒绝接管。状态查询只读，能否以普通用户查询还取决于系统 D-Bus 与文件读取策略；变更操作要求已有 root 权限，不自动 sudo/UAC。当前不支持 macOS、OpenRC 或 Windows 服务管理。

启停和重启默认最多等待 15 秒（`--timeout` 可调整），超时显示“未确认”，systemd 可能仍在继续操作。服务状态只说明进程/单元状态，结果 health 为 not-checked；它不保证 Hook 业务健康，仍需检查状态接口。卸载要求先停止服务，清理单元与相应启用关系，保留程序、配置、脚本、备份、日志及自定义运行目录。卸载后 systemd 重载失败会明确报告部分完成，不误报全部成功。
