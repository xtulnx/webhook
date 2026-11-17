# CLI 管理能力评估与开发计划

状态：分阶段实施中。P1 的 hooks list/show/validate 与 P2 的离线增删改、备份及协作锁已实现；P4 的本地 test/explain 已实现，Scalr 预演与逐节点 trace 尚不支持。P3 的本地刷新和 init 已实现（当前通过总配置关联常驻 Hook 实例，纯 Agent 接入待 P7）；P5 的 Linux systemd 代码已实现并通过模拟策略测试及构建，实机验收待完成；P6/P7 尚未实现。更新日期：2026-11-17。已移除交互向导，改为覆盖全局、Hook、Hub/Agent/级联、Admin、TLS、本地上下文的分类模板；YAML 带注释，JSON 无注释。包含 explain、init、Windows 服务；test 已按直接指定上下文重新定义，增加 Hub/Agent 本地管理及分级生效方案。

## 结论

值得增加有限的本地管理能力：Hook 查询与校验、受控增删改及运行实例主动刷新、本地上下文 test/explain、配置 init、Linux systemd / Windows SCM 服务注册，以及 Hub/Agent 本地配置管理与有限热更新。

目标仍是“接收请求、校验规则、映射参数、执行命令”。管理子命令是执行后退出的辅助工具，不增加独立常驻进程、TCP 管理端口、数据库或账号/令牌体系。维持单二进制，使用标准库 `flag` 和现有依赖。为可靠刷新增加可选本机 IPC，沿用操作系统权限；这是此次明确增加的运行时能力，不能宣称服务端完全零改动。

不把其他平台服务管理、通用脚本执行框架或任务调度纳入本轮。Windows 服务需要有限的生命周期适配，test/explain 共用现有规则、参数提取及命令执行能力；辅助条件免除只存在于 CLI 测试入口，不改变线上规则语义。网络改动按字段分级，不承诺任意拓扑无损热切换。

## 硬约束：单可执行文件完成全部子功能

- 每个平台仅需对应的 `webhook` / `webhook.exe` 即可使用本计划交付的管理与测试能力，包括 init、配置查询/增删改/校验、test/explain、实例发现与刷新、Hub/Agent 管理和系统服务管理。
- 不新增配套 `.sh`、`.bat`、`.cmd`、PowerShell、Python、JavaScript 等功能脚本，不依赖脚本解释器、curl/jq 等外部辅助工具，不要求安装服务 wrapper、另一个管理程序或后台辅助进程。
- 不用“内嵌脚本后释放到临时目录执行”“首次使用下载 helper”规避约束。子命令逻辑使用编译进同一程序的 Go 代码实现；静态模板与默认配置编译内置，不依赖仓库或旁置资源目录。
- 服务注册、启停、重启、状态和开机自启设置均由本工具提供入口；Linux 直接对接系统已有 systemd 的原生管理接口，Windows 对接 SCM API，不要求用户另外运行 systemctl、sc.exe 或 PowerShell 才能完成本计划的标准流程。不会为了满足单文件要求另造服务管理器；目标系统自身的 systemd/SCM 和必要操作权限仍是平台前提。
- 允许工具生成配置、systemd unit、实例记录、日志、备份和测试临时数据；这些是数据/系统声明文件，不是额外可执行组件。生成 unit 的启动目标必须是同一个 webhook 程序，不经过 shell wrapper。
- Hook 配置原本引用的用户业务命令/脚本及其业务依赖仍由用户提供；单文件约束覆盖 webhook 新增子功能，不改变其“调用外部业务命令”的核心用途。test 直接调用选定 Hook 的命令，不额外生成测试驱动脚本。
- 仓库已有安装/发布脚本不作为新子功能的运行前提，本轮不为这些子功能补充脚本。开发构建/测试工具不随运行产物安装。

## 现状与实现约束

| 已检查的实现 | 对方案的影响 |
| --- | --- |
| `internal/app/webhook.go:Main` 已在服务初始化前分流 `agent`、`update` | 新增同级 `hooks`、`service`，保留 `webhook [参数]`；无需引入 CLI 框架 |
| `internal/app/config.go` 通过全局 flag 和 `os.Args` 加载总配置 | 不能直接调用它启动一套管理运行时；先隔离最小的配置读取与路径选择逻辑，不重写全部配置系统 |
| `internal/app/hook_sources.go` 已支持多文件、非递归目录、唯一 ID 校验和热加载 | 管理命令沿用来源规则；离线校验应报告所有选定来源的错误，不能沿用服务继续运行、保留旧配置的容错结果 |
| `internal/hook/hook.go` 已有解析、执行限制校验、规则求值和参数提取 | 复用已有能力；校验通过不等于命令必定成功，也不等于所有动态规则已被验证 |
| `internal/app/admin_store.go` 已有 CRUD，但依赖进程内快照和互斥锁 | 不能把它直接套上 CLI；进程内锁不能协调 CLI 与 Admin，写入前需要重新读取磁盘 |
| `writeHooksFile` 在重命名失败后尝试删除原文件再重命名 | 新 CLI 不能继承这一失败窗口；安全替换是开放写命令的前置条件 |
| Admin 保存会重新序列化 JSON/YAML，模板模式只读 | CLI 同样禁止改写模板，明确注释与排版不保留；还需防止未知字段被静默丢弃 |
| `hookHandler` 处理方法、请求体、触发规则，再调用命令执行 | 本地 test 应明确上下文假设与覆盖范围，复用执行前检查/映射及执行能力，不冒充真实 HTTP 链路测试 |
| 运维文档已有 systemd 示例，当前平台代码没有 Windows SCM 服务生命周期接入 | Linux 服务注册可以局部实现；Windows 服务安装不能仅增加一条注册命令 |
| Unix 信号调用 `reloadAllHooks()`，没有调用方回执；Windows `setupSignals()` 是空实现 | 可靠主动刷新不能只依赖信号；需要本机请求/响应及加载版本确认 |
| `/status` 只返回来源 basename、ID、时间等，没有实例身份或内容版本 | 不能以端口、文件名或 HTTP 200 判断刷新了正确实例与正确内容 |
| `resolveRealIP` 校验直接连接来源是否属于可信代理 | 生产保持不变；本地 test 的 remote-ip 是直接注入的有效来源，不验证或模拟真实代理链路 |

## 拟定命令与行为

以下均为拟定接口，当前版本不可用。参数放在具体子命令之后，延续标准库 flag 的使用习惯；ID 使用显式参数，避免位置参数截断后续 flag 解析。

### 1. 查询与校验：首批交付

```sh
webhook hooks list --config ./webhook.yaml
webhook hooks show --file ./hooks.yaml --id deploy
webhook hooks validate --config ./webhook.yaml
webhook hooks validate --file ./hooks.yaml
```

- 来源选择：`--config` 使用总配置引用的完整 hooks 集合；`--file` 只检查指定文件，两者互斥。只读操作不隐式搜索系统目录、不主动刷新运行实例。
- 总配置模式保留现有工作目录、默认 `hooks.json`、目录扫描、路径去重及模板语义，不把相对路径擅自改为相对配置文件。
- 管理命令可显式传 `--working-directory` 指定配置内相对路径及 explain 命令查找的基准，默认当前目录；这只为管理操作选择服务原本的工作目录，不修改服务器现有解析规则。检测到运行实例的工作目录不同则提示正确值，不默默改用另一组文件。
- `list` 默认只输出 ID、来源和必要摘要；`show` 默认隐藏已知 secret 字段，并提示其他任意字段也可能含敏感数据；需要完整定义时显式 `--show-secrets`，输出不得宣称完全脱敏。
- 查询和校验支持 `--json`，stdout 输出结果，stderr 输出诊断。退出码约定：0 成功、1 校验或操作失败、2 用法错误；写操作另以 3 表示磁盘已保存但刷新未确认，explain 以 4 表示有效请求被方法、访问控制或规则拒绝。
- `validate` 检查可读性、格式、空/重复 ID、既有执行限制；报告未知字段，静态检查能确定的问题。整体来源任一文件失败均返回非零。
- 命令路径/PATH、工作目录是否可用可作为当前用户环境下的诊断，不承诺服务账号环境相同。模板校验按现有机制读取环境变量和引用文件，不输出展开后的密钥。
- 查询的是磁盘配置，不声称代表运行进程已加载的配置。校验不执行命令、不监听端口、不启用 watcher、Admin 或组网。

### 2. 配置增删改：通过文件写入前置验收后交付

```sh
webhook hooks add --config ./webhook.yaml --file ./hooks.yaml --from ./new-hook.json
webhook hooks update --config ./webhook.yaml --file ./hooks.yaml --id deploy --from ./deploy.json
webhook hooks delete --config ./webhook.yaml --file ./hooks.yaml --id deploy
```

- 写操作必须显式指定总配置和目标文件，目标必须属于该配置的有效来源；检查变更后的整个集合，防止跨文件 ID 冲突。与只读命令不同，这里 `--file` 是选定集合内的写入目标。
- 首版只修改已存在的普通 JSON/YAML Hook 文件；不创建目录、改总配置、自动注册来源、迁移文件或支持批量跨文件事务。拒绝符号链接写入。
- `--from` 接受一个完整 Hook 对象；`add` 遇已有 ID 报错，`update` 遇不存在 ID 报错，并要求 ID 不变。暂不提供字段 patch、交互编辑器或自动重命名。
- 模板模式拒绝写入。检测原文件与输入中的未知字段，无法保证保留时拒绝写入；不静默丢弃数据。
- 明确保存会重新序列化目标文件，注释和排版可能丢失；保存前保留原始字节备份。备份和锁文件不得具有 `.json`、`.yaml`、`.yml` 后缀，避免被目录加载器读入。
- 所有写命令支持 `--dry-run`：校验候选配置并输出默认隐藏敏感值的变更摘要，不写目标、备份或锁文件。预览仅代表当时快照。
- 真实写入以命令本身作为操作意图，不加入每次都询问的交互流程；错误或冲突直接返回非零。

文件写入的最低要求：

1. CLI 与 Admin 复用同一小型文件事务辅助层，对同一来源使用跨进程协作锁；按确定顺序获取涉及来源的锁，校验期间重新扫描目录及读取文件。
2. Admin 不能再仅基于旧内存快照覆盖目标文件；事务内读取最新磁盘内容，保留其他 Hook 的已有修改，并维持成功保存后立即更新运行时的原行为。
3. 校验候选集合，备份成功后在同目录写临时文件，再进行平台支持的安全替换；写入失败保留原文件，禁止“先删原文件再重命名”的回退。
4. 既有文件的权限、所有者处理必须明确，不能因 CLI 以高权限执行而改变服务可读性；无法安全保留时失败。备份权限不宽于原文件，临时文件不扩大读取权限。
5. 提交前检查来源变化；已知冲突直接失败，不自动覆盖或重试。协作锁只协调新版 CLI/Admin，普通编辑器和旧版本程序不遵守锁；不能把摘要复核宣传为对所有外部写入的原子比较交换，也不承诺多个独立文件的原子快照。

如果这套最小文件事务无法在局部修改中完成，先发布只读能力，暂缓 CRUD。不要为此建立通用配置数据库或跨进程配置服务。

新增配置改为生成可编辑模板：`config template list` 查看用途，`config template <类型> --output <路径>` 新建文件；默认带中文注释的 YAML，也可选 JSON。单个 Hook 对象仍通过 add --from 进入既有事务及刷新；不增加交互框架或辅助脚本，也不自动把全局/网络模板应用到运行实例。

### 3. 检测运行实例与主动刷新

只有成功的 add/update/delete 执行默认的主动刷新；list/show/validate/explain 和 `--dry-run` 均不触发刷新。另提供 `hooks reload`，用于外部编辑后主动加载，或在保存成功但刷新失败后单独重试。

```sh
webhook hooks update --config ./webhook.yaml --file ./hooks.yaml \
  --id deploy --from ./deploy.json --reload auto
webhook hooks reload --config ./webhook.yaml
```

**发现范围与身份：**

- 服务增加可选 `runtime-dir` 参数/配置项。未配置时保持旧部署行为，不创建控制通道；`config init` 生成此项，`service install` 为受管服务明确配置并展示其运行目录。
- 运行目录只保存受操作系统权限保护的实例描述：随机实例 ID、PID/启动身份、总配置规范路径、实际工作目录、有效来源描述和 IPC 地址。原始密钥、请求数据不写入此记录。
- CLI 从明确配置的运行目录或本工具的服务安装记录发现实例；可用 `--runtime-dir` 显式选择。安装记录与总配置冲突时报错，不自行猜测。无总配置的前台实例可通过显式运行目录及实例 ID 执行 reload，但不由 CRUD 自动关联。
- 不遍历所有进程、不扫描端口、不凭进程名/PID 文件给进程发信号。既核对配置路径，也核对实际工作目录、有效来源（包括启动参数追加的来源）与握手身份；不匹配时停止自动刷新。
- 多个匹配实例要求 `--instance` 明确选择，不广播；提醒同一文件被其他实例引用时，它们可能通过各自 watcher 自动加载，选择实例不提供运行时隔离。
- 已知多实例歧义、权限不足、来源不匹配等在写文件前报告并停止；没有发现实例与“系统中绝无该服务”是不同结论。旧版本或未启用 IPC 的服务报告不能确认，保留原有 watcher/人工重载路径。

**刷新方式：**

- Unix 使用独立的 Unix socket，Windows 使用专用 named pipe；均在原进程内处理，不复用业务 `-socket`，不开放 TCP 端口，不要求开启 Admin。
- 通道只提供身份/加载版本查询、hooks reload，以及第 7 节明确的 network status/reload，不接受任意文件读取、任意配置路径、命令执行、升级或整个进程重启请求。调用方只能请求重载进程自身已配置的来源。
- 运行目录、实例记录及端点要求安全的所有者/权限。Unix 限服务用户、明确授权管理者及系统管理员；Windows 使用显式 ACL 且拒绝远程管道客户端，不使用宽松默认 ACL。无权限时明确报错，不静默提权或放宽权限。Windows named pipe 的默认 ACL 不适合直接当成本机管理授权，见 [Microsoft 文档](https://learn.microsoft.com/en-us/windows/win32/ipc/named-pipe-security-and-access-rights)。
- 安装工具只创建自己的运行状态目录并设置必要权限，既有业务目录不自动改权限。前台模式创建失败时显式报错；IPC 默认关闭时不影响原启动方式。退出清理自身记录，崩溃残留通过身份握手判废，不据此向复用 PID 发控制命令。
- 保存并释放文件事务锁后，CLI 发出一次刷新请求，携带刚保存文件的原始内容摘要与实例 ID。服务从磁盘读取、校验自己的完整候选集合；成功后一次性发布候选快照，并回传实例 ID、加载 generation、目标文件摘要及错误信息。
- 主动 reload 全集合失败时保留刷新开始时的运行快照，不部分发布。已存在的 watcher 可能在此之前生效，所以不能声称“刷新失败意味着刚才的修改从未生效”。
- watcher、Admin、主动刷新共用加载/发布的串行协调边界，避免旧读取覆盖新快照；保留 watcher 现有触发范围与容错策略。记录加载元数据用于确认，不往公开 `/status` 暴露内容摘要。必要的锁顺序、短临界区和请求超时在此阶段测试。
- watcher 已加载相同内容时，主动请求可直接确认，避免重复发布。成功只表示回执时该版本已加载，新请求使用新快照，在途命令不被重启或取消；随后的外部修改仍可产生新版本。
- hooks reload 不涉及总配置、监听地址、TLS、Admin 密钥和组网拓扑。network reload 的字段范围另见第 7 节；其余进程级配置变更提示需要重启，不自动重启。

**结果与失败语义：**

| 模式/结果 | 行为 |
| --- | --- |
| `--reload auto`（默认），找到唯一匹配实例 | 保存后主动刷新并等待回执；校验目标摘要后才显示“已生效” |
| auto，在允许的发现范围内没有实例 | 保存成功，输出“已保存；未发现可刷新的运行实例，运行时未确认”，退出 0 |
| `--reload required` | 保存前必须存在可访问的匹配实例；保存后还必须收到正确回执。前置检查失败不写文件；保存后的失败返回 3 |
| `--reload never` | 只保存、不主动刷新；明确现有 hotreload 仍可能自动生效，不承诺保持运行时不变 |
| 已保存，但实例退出、刷新拒绝、版本冲突或等待超时 | 返回 3，输出 `saved=true`、`reload=failed/unconfirmed`；保留文件与备份，不自动回滚磁盘 |

`hooks reload` 本身不写配置，失败返回 1。CLI 默认等待 5 秒，可用 `--reload-timeout` 调整；超时后服务可能仍完成加载，输出“未确认”，不能显示“未执行”。后续只重试 reload，不建议重跑 add/delete。JSON 输出与人类可读结果遵循同一语义。

### 4. 服务安装与卸载：Linux systemd 与 Windows SCM

共同要求：固定管理一个名为 webhook 的受管服务；安装注册与启动分开，默认手动启动；显式记录配置、工作目录、程序路径和运行目录，避免依赖启动管理器的默认工作目录。已有非本工具服务拒绝接管；卸载保留业务配置、程序、脚本、日志和备份。不自动调用 sudo/UAC、不安装第三方 wrapper。

为了满足单程序完成标准流程，以下命令由原先可选项调整为服务管理的必要范围：

```sh
webhook service start
webhook service stop
webhook service restart
webhook service status --json
webhook service enable
webhook service disable
```

这些入口只操作本工具管理的 webhook 服务，委托原生服务管理器；不增加进程托管框架。enable/disable 仅改变开机启动设置，start/stop 仅改变当前运行状态。start/stop/restart 有界等待并报告实际结果，status 区分服务运行状态与 Hook/网络业务健康。restart 是用户显式操作，不由配置保存自动调用；不支持对任意前台 PID 执行 service stop/restart。

Linux systemd：

```sh
webhook service install --config /etc/webhook/webhook.yaml \
  --working-directory /var/lib/webhook --user webhook --dry-run
webhook service install --config /etc/webhook/webhook.yaml \
  --working-directory /var/lib/webhook --user webhook
webhook service uninstall
```

- 首版固定管理 `webhook.service`，仅生成普通前台服务单元并注册；不覆盖用户已有单元，不接管 socket 激活部署，不支持多实例。
- 显式指定已存在的运行用户和工作目录，程序及总配置记录绝对路径。正确处理 systemd 参数中的空格、引号及特殊字符，不拼接 shell 命令执行。
- `--dry-run` 输出拟生成单元、目标位置和后续动作，无系统写入；真正安装要求已有权限，不自动调用 sudo、创建用户、复制二进制、安装软件包或更改既有业务目录权限。
- 安装默认不启动、不设置开机自启；提示使用 `webhook service enable` 和 `webhook service start`，保持注册与执行业务命令分离。
- 已有完全一致且带管理标记的单元可幂等成功；不同内容或无管理标记时拒绝覆盖。单元写入或 daemon-reload 失败需报告准确状态，并尽可能撤销本次新建内容。
- 卸载仅处理本工具生成、内容可确认归属的单元；服务仍在运行时要求先运行 `webhook service stop`，不强杀进程。清理对应启用关系及单元并通知 systemd 重载，保留程序、配置、脚本、日志、备份和更新状态。
- Linux 使用原生 systemd D-Bus 接口完成注册后的重载、状态查询和启停等操作，不执行 systemctl 或 shell 脚本。优先评估现有 go-systemd 模块相应包；当前 go.sum 仅有 godbus 的 go.mod 校验项，具体启用包可能增加编译依赖与体积，实施时锁定并评估必要增量，不声称完全零依赖变化，也不增加运行时安装组件。
- 不绑定 `webhook update`，不自动升级或重启。安装可为本工具的运行状态目录配置必要权限，不自动修改业务目录权限。
- Linux 没有 systemd 或已有复杂 unit/drop-in/socket 关联时明确报不支持或冲突，不尝试通用兼容回退。

Windows SCM：

```powershell
webhook.exe service install --config C:\Webhook\webhook.yaml --working-directory C:\Webhook --dry-run
webhook.exe service install --config C:\Webhook\webhook.yaml --working-directory C:\Webhook
webhook.exe service uninstall
```

- 使用现有 `golang.org/x/sys/windows/svc` 与 `svc/mgr` 能力实现原生服务；实现前验证锁定版本 API，优先不升级依赖。安装登记同一 exe 的专用服务入口，前台 `webhook.exe [参数]` 行为保持兼容。
- 必须处理 StartPending/Running/StopPending/Stopped、Stop/Shutdown、Interrogate，初始化完成后再报告 Running；启动错误、端口占用和退出原因可诊断。状态上报是原生服务的基本要求，见 [Microsoft 服务状态文档](https://learn.microsoft.com/en-us/windows/win32/services/service-status-transitions)。
- 从当前 `Main` 提取最小可停止运行入口，错误向外返回，由 CLI 或 SCM 适配层决定退出；避免底层 `os.Exit/log.Fatal` 绕过 SCM 状态与清理。不整体重写 flag、路由、网络或执行器。
- 停止时停止接受新工作，关闭监听、watcher、本机控制通道，取消组网连接，清理 PID/实例记录。设置有界停止过程，正确上报状态；不承诺等待所有异步业务命令或终止全部派生进程，不顺带引入任务管理器。实际行为必须在文档与 Windows 测试中明确。
- 新安装默认使用 `NT AUTHORITY\LocalService`，不默认使用 LocalSystem。该账号权限较低且对网络呈现匿名凭据，不能等同于当前登录用户，见 [Microsoft LocalService 文档](https://learn.microsoft.com/en-us/windows/win32/services/localservice-account)。部署脚本所需的文件、PATH、网络凭据由运维准备；账号不合适时使用系统服务管理工具配置，本轮不增加密码参数或账号管理功能。
- 持久化日志使用已有日志能力，登记明确的可写日志路径，启动失败至少提供 SCM 错误；不依赖可见控制台。只为本工具创建的状态/日志目录设置 ACL，不自动授予业务目录写权限。
- Windows 原生路径、带空格的 exe/配置路径、Unicode、正确参数引用与跨账号 ACL 为必测项。已有服务不同配置不覆盖，运行中不卸载，删除待完成状态如实报告。
- 默认手动启动，统一通过 `webhook.exe service start/stop/restart/status/enable/disable` 操作；内部直接调用 SCM，不执行 PowerShell、sc.exe 或额外 wrapper。服务本身必须响应系统启停。
- hooks 刷新使用上述 named pipe 回执协议，不依赖 Unix 信号，不把 SCM “接受控制请求”当成配置已生效。

macOS launchd、OpenRC、多个服务实例和带密码的服务账号安装暂不纳入。Windows 实机启动、停止、刷新、卸载全部通过才算完成，交叉编译只能验证构建。

### 5. 直接指定上下文的本地 test / explain

本节按用户澄清重新定义：test 是对选定 Hook 的本地功能测试，直接注入“已经收到请求/文件”的上下文，不构造或发送 curl/HTTP 请求，不需要运行中的服务。explain 使用同一上下文与判定过程，只解释不执行；test 可在检查后执行真实命令。两者不验证网络传输、代理、TLS 或 Hub/Agent 握手。

```sh
# 直接指定有效远程 IP、query 和已上传文件；实际执行命令
webhook hooks test --config ./webhook.yaml --id deploy \
  --remote-ip 192.0.2.10 --query env=staging \
  --uploaded-file artifact=./package.zip --execute
# 不指定远程 IP：IP 条件按“假定满足”处理，不编造一个白名单 IP
webhook hooks test --config ./webhook.yaml --id deploy \
  --query env=staging --uploaded-file artifact=./package.zip --execute
# 对同一份上下文只解释，不执行
webhook hooks explain --config ./webhook.yaml --id deploy --context ./test-context.yaml
# 严格模式不假定缺失条件满足
webhook hooks explain --config ./webhook.yaml --id deploy --context ./test-context.yaml --strict
```

**上下文输入：**

| 输入 | 语义 |
| --- | --- |
| `--remote-ip` | 直接指定规则使用的有效来源 IP，支持 IPv4/IPv6；无 TCP 连接，不再经代理头推导。不是系统环境变量，也不修改服务配置 |
| `--method`、`--header`、`--query` | 直接指定方法及键值，header 名按现有规则规范化；显式空字符串与未提供区分 |
| `--body-file` + `--content-type` | 指定原始字节及解析方式，复用 JSON/XML/form 解析；支持原始 body 的规则和参数引用 |
| `--payload-file` | 直接指定已经解析的 JSON payload，跳过传输解码；与 body-file 互斥，不据此伪造原始签名字节 |
| `--uploaded-file 字段名=本地路径` | 把文件作为已上传文件输入；上下文文件可进一步指定原文件名、Content-Type，支持同名字段多文件 |
| `--context` | JSON/YAML 上下文文件，字段为 remote-ip、method、headers、query、body-file/content-type 或 payload-file、uploaded-files；与逐项上下文 flag 互斥，避免隐含覆盖 |

上下文文件中的文件引用相对上下文文件目录，直接 CLI 路径相对当前目录；Hook 的相对命令/工作目录仍遵循总配置与 `--working-directory` 规则。uploaded-files 可与结构化 payload 组合，表示同一次已解析输入。原始 body 与独立文件列表并存时不声称两者来自同一个真实 multipart 包；需要验原始签名时必须另有对应原始 body。

不支持 `--url`、curl 文本导入、伪造网络连接或自动选择线上实例执行。header 中的 X-Forwarded-For 仅是可被 Hook 引用的数据，不覆盖显式 remote-ip；测试代理信任链应使用真实集成测试。

**默认辅助模式：未提供的检查条件假定满足，明确提供的条件真实求值。**

- 条件结果区分 `passed`（真实满足）、`failed`（真实拒绝）、`assumed`（缺失上下文，免除该项检查）、`skipped`（短路未求值）、`error`。对外明确标注“本地辅助测试”，不得把 assumed 展示成已验证。
- 未提供 remote-ip：服务级 IP 访问控制与 Hook IP 条件标为 assumed；提供后按实际配置判断。未提供 method 同理；指定不允许的方法则拒绝执行。
- value/regex 等条件的输入参数未提供时 assumed；显式空值、错误值仍执行判断。缺失条件不会从配置中反推并写入 payload/query，也不会生成看似真实的请求值。
- 签名条件的签名输入未提供时 assumed，不自动代签。明确提供签名时必须有该规则所需的原始 body/时间等上下文，否则报缺失输入；签名错误按现有规则/soft-failure 处理，不能继续当作 assumed。
- 原始 body、payload、上传文件提供后内容损坏或类型不符属于错误；非法 IP/CIDR、正则、配置、文件不存在/不可读也不能以“默认通过”掩盖。静态配置检查先于辅助求值。
- 对已提供的条件保留 AND/OR/NOT 与短路语义；合法 OR 的其他真实成功分支仍可使总体通过。显式失败不能单靠一个缺失分支而被改成通过。

辅助模式必须定义组合条件的免除语义，不能简单把每个缺失叶子替换成 true：

| 组合 | 辅助模式判定 |
| --- | --- |
| `AND(assumed, passed)` | 真实部分通过，可继续；保留 assumed 记录 |
| `AND(assumed, failed)` | 拒绝 |
| `OR(assumed, failed)` | 拒绝；缺失分支不掩盖真实失败 |
| `OR(assumed, passed)` | 可继续，按既有短路顺序记录 |
| `NOT(assumed)` | 整项免除，不把 assumed 当 true 后取反成 false |
| 一组条件全为 assumed | 整组免除，可继续；不能据此宣称存在满足全部条件的真实请求 |

实现为测试专用的“未提供检查免除”层：在保留原始规则用于解释的同时，构造仅含可评估条件的临时候选树，再调用现有 evaluator；空子树传播免除状态，NOT 的免除不取反。只对显式命令入口启用，不将辅助开关放进线上 Hook schema、HTTP header 或公共运行配置。生产规则 evaluator 的默认语义保持不变。

`--strict` 关闭免除：缺失规则输入不得成为 presumed success，按现有规则及缺失参数/soft-failure 行为报告；缺失 method/IP 这类无法构造确定判断的上下文时返回输入不完整。严格模式仍是本地上下文测试，不代表验证了真实网络/账号环境。

**检查条件与命令数据分开：**

- “默认通过”只免除缺失的触发条件，不创造命令所需的参数、环境变量内容或文件。如果 argv/env/pass-file 映射依赖未提供的数据，test 在执行前报具体缺项；explain 可完整列出问题并返回非零。现有生产执行器的容错行为不因此改变。
- 例如 IP 白名单缺少 IP 可以继续，但命令同时引用远程地址时必须提供；query.env 只用于触发规则可免除，若还作为发布目录参数则必须填写。不可根据白名单、正则、secret 推测业务数据。
- 不指定文件不会得到空文件或假路径。只有规则依赖的缺失文件条件可免除，命令实际读取/映射所需的文件缺失必须报错。

**已上传文件与真实执行：**

- 本地文件输入转换为与已解析上传等价的内部文件记录，不编码 multipart、不模拟上传过程。使用文件访问适配层共用现有文件解析/执行映射，避免依赖不可直接构造的 `multipart.FileHeader` 内部字段；必要时局部提取上传文件接口。
- 复用当前 `parse-parameters-as-json`、`keep-file-environment`、`pass-file-to-command` 语义；同名字段/文件仍按当前生产实现使用的项处理，不借测试功能悄悄扩大线上能力。明确区分上传文件与由 payload 生成的环境文件。
- 执行前将要暴露给命令的上传文件复制到本次测试专用临时目录，保留指定原文件名等元数据；避免执行器清理/写入上传路径时操作用户源文件。成功、失败、取消后清理本次创建的临时文件，不删除输入文件。此隔离不是脚本沙箱，脚本仍可做其账号有权执行的操作。
- test 有 `--execute` 才执行；缺失时显示解释结果和未执行状态，不交互追问。explain 永远不执行，也不创建给命令使用的临时文件；允许读取文件内容/元数据以完成解析与校验。
- 使用当前 CLI 用户与环境，在结果中展示实际工作目录和执行身份；不伪装成服务账号、不经 IPC 交给服务执行、不占用正在运行服务的并发槽。仅运行一次并同步等待，不重试、不改 Hook 原有同步/异步配置。
- 共用现有命令准备与执行能力，通过显式参数选择测试调用方式；仍是同一命令、argv/env/文件映射。test 同步等待仅影响本次 CLI，线上异步响应行为不变。不得另造 shell 命令拼接或通用脚本执行框架。
- test 默认 30 秒上限，可用 `--timeout` 显式设置正时长；与 Hook/全局配置的非零命令超时取更短值。Ctrl+C 取消本次执行，按现有平台能力终止直接子进程并清理资源；不承诺清理任意派生进程树。
- explain 结果默认隐藏请求派生值、只展示变量名/映射摘要，`--show-values` 可展开，配置 secret 始终隐藏。test 的命令输出可能含脚本自行打印的数据，不能声称全面脱敏；JSON 中单独放置输出，不能与结构化结果混流。
- 退出码：0 为解释可执行/实际命令成功，1 为输入、映射、超时或命令失败，2 为用法错误，4 为已评估条件拒绝。结构化结果包含 assisted/strict 模式、assumptions、executed、commandExitCode、timedOut；不直接用业务程序任意退出码覆盖 CLI 分类。

explain 与 test 对同一上下文必须得到相同的条件判定及映射结果；输出应区分“辅助条件下可执行”与“全部条件真实通过”。测试命令不自动刷新配置、不加载线上快照、不验证 Hub 路由权限、TLS、真实代理链路或服务级跨进程并发。

### 6. 最小配置初始化 `config init`

```sh
webhook config init --dir ./webhook-local --dry-run
webhook config init --dir ./webhook-local
```

- 创建最小总配置、空 Hook 数组文件及简短使用说明；不复制完整参考配置、不生成部署脚本、不绑定业务发布操作。
- 默认仅监听 `127.0.0.1:9000`，Admin/组网关闭；启用显式 Hook 文件 hotreload，并设置受控 `runtime-dir`，便于后续主动刷新。配置 `nopanic: true` 以允许空 Hook 启动，说明此时 `/status` 可能为 degraded，这是尚未添加 Hook 的预期状态。
- 生成的 Hook、运行目录引用使用基于目标目录的绝对路径，保留现有路径解析语义；提示搬迁目录后需更新路径。服务工作目录在安装时仍需显式指定。
- 任何目标配置/说明文件已存在均拒绝覆盖，不提供隐式合并或首版 `--force`。允许已有空目标目录；逐个使用排他创建，失败仅清理本次新建且确认未被外部修改的产物，不删除原有内容。
- `--dry-run` 展示将创建的路径与配置，不创建目录/文件。实际创建不启动服务、不注册系统服务、不下载程序、不生成固定测试密钥。
- 新配置文件使用用户私有权限，运行目录由服务按安全策略创建；Windows 应校验 ACL。安装到不同服务账号时明确检查访问权限并提示所需准备，不自动放宽业务配置权限。

### 7. Hub / Agent 本地管理与分级动态生效

可以提供与 hooks 一致的“查询 → 校验 → 变更预览 → 保存 → 刷新确认”，但网络配置同时控制认证、连接和监听，不能把所有变更统一称为无中断热加载。本轮优先本地配置管理及可隔离的热更新；需要重连的能力单独阶段交付，整体模式切换仍要求人工重启。

**代码现状：** `NewHub` 启动时建立 nodes/routes map，HTTP 路由也在 `registerNetworkHookRoutes` 启动时注册；`RunAgent` 创建固定配置、services、并发信号量与 forwards 监听。当前没有网络配置热更新入口。已存在的约 2 秒 Hook inventory 更新只传播 Hook 元数据，不等于 Hub/Agent 配置可重载。部分 map 读依赖“配置不变”的假设，不能直接替换字段然后声称线程安全。

命令统一置于 `network`，不占用现有 `webhook agent [参数]` 临时入网入口：

```sh
webhook network show --config ./webhook.yaml
webhook network validate --config ./webhook.yaml
webhook network status --config ./webhook.yaml
webhook network hub nodes add --config ./webhook.yaml --from ./node.yaml --dry-run
webhook network hub nodes update --config ./webhook.yaml --id node-a --from ./node.yaml
webhook network hub routes update --config ./webhook.yaml --path /deploy --from ./route.yaml
webhook network agent show --config ./webhook.yaml
webhook network agent update --config ./webhook.yaml --from ./agent.yaml --dry-run
webhook network reload --config ./webhook.yaml
```

- Hub 的 nodes/routes 支持 list/show/add/update/delete；nodes 以 ID、routes 以 path 定位。Agent 本身是当前进程的单例配置，提供 show/update；其 services/forwards 支持 list/show/add/update/delete，以 name/listen 定位。模式创建/切换不伪装为增删一个列表项。
- 全部命令支持中文帮助、结构化结果和默认脱敏。`network show` 表示磁盘期望配置，`network status` 经已有本机 IPC 查询实际 generation、连接/注册状态、活动会话、待重启差异及最近刷新结果，不要求开 Admin。
- 读取、校验和显式 reload 支持现有内联 `network:` 与外部 `network-config` 两种来源，沿用各自的相对路径语义；首轮自动 CRUD 只改外部网络配置文件，避免改写整份总配置。内联模式提示手动编辑后 reload，不自动搬迁或重写无关字段；内联自动编辑另评估。
- 写操作复用 P2 文件事务、备份、权限、冲突检测、P3 身份确认与 `--reload auto|required|never`。只修改当前本机配置，不替远程节点写文件，也不把 Hub 的节点清单当作远程机器管理入口。
- node/route/service 删除前校验全部引用。存在引用则拒绝，不自动级联删除。提供完整 `network update --from` 作为一个文件内的候选配置整体替换，用于同时移除互相依赖的条目；不增加多机或多文件事务。Hub 最后一个 node 删除仍遵守现有至少一个 node 的校验要求。
- 变更预览除字段差异外，还显示 `hot`、`reconnect`、`restart-required` 类别、受影响节点/监听/会话以及是否撤销权限；不能仅展示“文件保存成功”。预览不承诺到执行时连接数量不变。

**生效分类（目标能力，当前均须开发验收）：**

| 配置变化 | 生效方式与范围 |
| --- | --- |
| Hub hook_routes 新增/修改/删除、路由 IP 限制、hook_prefix | 热发布配置快照；新请求按新路由/策略判断，已接收 HTTP 请求使用原请求快照完成。删除后的路径回归既有路由优先级，若可能落入同名本地 Hook，预览必须指出 |
| Hub request_timeout、max_sessions | 热更新作用于新请求/会话；下调容量不强杀已存在会话，达到新上限前拒绝新增 |
| Hub 新增节点、扩展授权 | 热更新本机授权策略，不主动配置远端；已有 Agent 要发布新增服务时仍需它自己的配置更新/重连 |
| Hub 删除节点、token 轮换、缩减 services/allow | 热更新同时撤销受影响的控制连接、待配对会话和已建隧道；这是会中断受影响业务的安全策略生效，不承诺无中断。已派发的业务命令不保证被取消 |
| Agent 同名 service 的 target/relay_to，已有 forward 的 to | 热更新只作用于新的连接，保留既有流；relay_to 必须仍满足当前本地 Hub 引用约束 |
| Agent server、id、token/CA、services 名单、forwards 监听集合、max_connections | 受控重建 Agent 网络组件并重新注册；可能中断隧道或短暂无法连接，不能叫无损热更新 |
| mode、with_webhook、webhook_only_via_hub、allow_public_listen、业务监听/TLS 等进程结构或暴露面 | `restart-required`，首轮不动态切换，不自动重启整个进程 |

标为 hot 并不代表已经存在更新 API；P7 仅在快照/鉴权/路由并发测试通过后开放对应字段。Agent 需重连的类别列为 P8 候选，不默认纳入首轮；P8 未交付时明确 `unsupported-live-change`，可通过 `--reload never` 保存后显式执行 `webhook service restart` 重启受管服务，不得悄悄自动重启整个服务。前台进程仍由用户退出后使用同一程序重新启动，无需额外脚本。

**刷新、撤权与运行边界：**

1. hooks reload 和 network reload 分开，只作用于指定子系统；network 不新增文件 watcher，避免普通文件保存隐式触发凭据轮换或断连。network CRUD 成功后才走主动刷新，外部编辑使用显式 reload。
2. 网络有效快照应包含已解析策略、源配置以及引用凭据/CA的版本信息。token_file 内容变化必须能被识别；不得只比较 YAML 文件摘要。token_env 由服务当前进程环境解析，CLI 的环境不同不能被当成在线生效证明。摘要及密钥不暴露到公开状态接口。
3. 本地 IPC 增加固定的 network status/reload 能力，不接受对端指定任意路径、任意网络命令或执行脚本；纯 Hub、纯 Agent、both 和前台服务都须能启用它，覆盖当前纯 Agent 的提前返回启动分支。
4. Hub 使用一致的不可变配置快照或受控同步访问；单次认证/路由选择使用同一 generation。握手完成前和待配对会话接入时重新确认授权 generation，防止撤权与认证竞态让旧凭据重新接入。保留重放防护记录，不因刷新清空重放窗口。
5. Hub HTTP 动态路由通过稳定分发入口查询当前快照，不在已运行的 mux 上并发追加/删除路由。维持 network 路由优先于本地 Hook、状态/Admin 保留路径、访问控制等现有语义，不重写全部 HTTP 路由。
6. 撤权范围精确到受影响节点/服务/目标会话，其他连接保留；状态记录同步更新，不保留已删除节点为可授权节点。无法确认撤权已落实时回执不得显示 applied。网络连接关闭不能撤销已经发生的业务副作用。
7. 默认只自动应用无需中断的 hot 变更。涉及撤权/重连的变更必须显式 `--allow-disruption`，预览列出影响；这是 CLI 功能参数，不是额外交互确认。与 required 类似，已知不支持的模式变更或缺少该参数在写盘前拒绝；显式 `--reload never` 可仅保存待人工生效。
8. 外部已编辑配置后的 reload 若含未允许的中断或 restart-required 字段，拒绝整批刷新，保留旧运行快照并报告；不只应用其中一半 hot 字段。`both` 的 Hub/Agent 交叉引用先做完整候选校验，涉及双方的混合变更首轮要求重启，不承诺跨组件事务。
9. 自动模式下没有匹配运行实例可保存；存在实例但刷新失败时沿用退出码 3 与 saved/applyState 分离，磁盘不自动回滚。明确区分 `saved`、`applied`、`reconnecting`、`online`、`restart-required`、`failed/unconfirmed`，不会把“重连已开始”显示为“已在线”。
10. 若选择 P8，Agent 受控重建先校验候选、读取 CA/凭据、预检可检查的资源，再停止旧网络组件并释放对应监听/连接，按新配置重建。停止必须有界且能结束活动隧道，不能直接调用现有可能等待活动 goroutine 的退出逻辑后无限阻塞。独立的本地 webhook HTTP 服务继续运行，但经 Agent 的业务会受影响。
11. 重建失败可尝试恢复上一份内存配置及监听，但不保证旧 Hub/端口仍可用；回执分别报告恢复结果与磁盘期望版本。恢复不了则明确 degraded，不能吞掉失败或误报旧状态。成功需要区分本地资源就绪与对端注册确认；当前协议没有充分确认时增加向后兼容的能力协商/ack，旧 Hub 最多显示注册未确认。
12. Agent 修改 ID/server/token 不替 Hub 同步授权；凭据轮换由操作者分机协调，不承诺双端原子更新。与已有临时 `webhook agent` 的生命周期隔离，首轮不修改临时进程的到期时间、share 或启动参数。

**与 hooks 刷新的关系：** Agent 本地 Hook 已加载并不等于上游 Hub 已更新 inventory。回执默认只承诺本地加载，展示上游传播为异步/未确认；现有定时 inventory 机制继续工作，不为此承诺跨级联全链路即时一致。

## 分阶段交付与验收

每阶段独立提交、可独立停下；进入下一阶段以前完成当前范围的测试与文档。

| 阶段 | 修改范围 | 验收重点 |
| --- | --- | --- |
| P1：查询与校验 | 新增命令及小范围提取配置来源读取 | JSON/YAML、多文件/目录、默认来源、模板、重复 ID、无效来源；旧启动方式及 `agent/update` 帮助不变 |
| P2：配置写入 | 文件事务辅助层，局部接入 Admin，CRUD | 两个 CLI/CLI 与 Admin 同时写时不静默覆盖；旧内存快照不丢失其他修改；未知字段、模板、链接拒写；备份、Windows ACL/Unix 权限及替换失败保留原文件；热重载仍识别替换 |
| P3：实例发现、主动刷新与 init | 受限 IPC、实例记录、加载协调和回执，接入 CRUD，生成最小配置 | Unix socket/Windows pipe 权限与身份；残留记录、多个实例、同名异路径文件、CLI 附加来源、重启竞态；旧服务兼容；auto/required/never；全候选失败保留快照；watcher/Admin 竞争不回退；超时与 saved-but-unconfirmed 结果；init 不覆盖、失败不伤原文件、生成后可启动并被发现 |
| P4：本地上下文 explain/test | 上下文适配、缺失检查免除、文件适配，共用规则/参数映射/执行器 | 未指定 IP 默认免除、指定 IP 拒绝/通过；未提供与显式空值；AND/OR/NOT 和签名缺失/错误；命令必需数据缺失拒绝；上传源文件不被清理；严格模式、超时/取消/输出/退出码；无 HTTP 发送；线上规则与同步/异步回归 |
| P5：systemd | 原生服务管理接口、Linux 平台文件、固定单元与运行目录 | 参数转义、幂等、冲突、失败清理和卸载边界；仅用 webhook 完成 Linux 实机安装/启停/重启/状态/自启设置/刷新/卸载；配置与工作目录身份匹配 |
| P6：Windows SCM | 最小可停止运行入口、原生 SCM 适配、账号/日志/运行目录处理 | 仅用 webhook.exe 完成 Windows 实机安装/启停/重启/状态/自启设置/刷新/卸载；后台无控制台；低权限账号、路径含空格/Unicode、管道 ACL、配置/日志不可读写、启动失败状态、停止期限；普通前台与 Linux 回归 |
| P7：Hub/Agent 本地管理与有限热更新 | network 命令、外部配置 CRUD、IPC status/reload、Hub 快照/动态路由、Agent 同名目标更新 | 来源/模式/引用校验；inline 只读和手动 reload；撤权关闭受影响会话与重放保护；握手/刷新竞态；新旧请求快照、路由优先级、容量下调；磁盘/运行时差异；未支持变化拒绝热应用；纯 Agent IPC 可用 |
| P8：候选后续，不默认实施 | Agent 网络组件受控重建、注册确认与必要能力协商 | 监听释放、活动隧道有界退出、重连/注册确认、资源冲突、失败恢复；老 Hub 兼容；本地 Hook 服务保持运行；不承诺分布式原子提交 |

依赖顺序为 P1 → P2 → P3；P4、P5、P6 可分别交付，但用户要求的 Windows 服务仍属于承诺范围，不能因排序靠后而降为可选。完整 init 与 runtime-dir 支持同批交付。P2 在刷新未交付时只能明确标为离线文件管理，不能提前宣称自动生效。

P7 依赖 P2/P3 并按“读/校验 → 文件管理 → 按字段开放热更新”交付。Agent 可停止生命周期复用 P6 所需的基础能力；P8 的成本高于普通配置刷新，作为用户可决定的后续项。若网络热更新需要显著重写协议或连接管理，停止扩大该阶段，仅交付已验证范围并报告不支持字段。

实现后默认执行 `go test ./...`；共用加载/发布变化增加针对性并发及 race 检查。变更涉及的平台执行构建验证，systemd/SCM 分别需要真实 Linux/Windows 环境验收，不能用 macOS 测试或交叉编译替代。使用现有安全测试命令，不触碰真实发布 Hook。每阶段比较同构建参数的二进制体积与依赖变化，默认不新增第三方依赖。

增加单文件交付验收：将目标平台二进制单独复制到不含源码、模板和配套脚本的目录，使用其完成标准管理流程；不安装 Go/Python/Node/PowerShell 等运行环境，不从 PATH 调用功能辅助工具。测试 Hook 的业务命令使用独立准备的无副作用测试样例，不计作工具运行依赖。检查管理/test 适配层不会生成并执行脚本或下载 helper；验证内置模板、配置生成、原生服务接口和 IPC 均可用。Linux 构建检查额外动态库依赖，优先使用不增加外部运行库的构建方式。

文档随功能阶段更新 `docs/Webhook-Parameters.md`、`docs/Configuration.md`、`docs/Operations.md` 及示例。本文件是方案，不先把拟定命令加入已支持功能列表。

## 本轮明确不做

- CLI 框架迁移、服务主流程整体重构、改变现有业务配置格式或引入持久化数据库；测试上下文只作为工具输入。
- 多主机远程配置管理、远程 Admin 登录客户端、自动分发令牌、TCP 管理端口；IPC 仅为本机身份/状态确认与限定配置刷新。
- 内置守护进程、定时/排队/重试任务、执行历史中心、发布编排。
- 在生产 HTTP/组网入口暴露测试用假定通过、指定身份或免鉴权能力；自动修改持久化 Hook 为同步执行。本地 test 的明确辅助语义与生产执行分离。
- 安装时修改防火墙、反向代理、创建系统账号、调整业务目录权限或公网绑定；允许为本工具新建状态/日志目录设置必要权限，并指定已有服务账号。
- 自动重启、自动更新、自动删除用户配置和业务数据。
- 在本轮顺带修复所有历史行为；发现问题单独记录，仅修复新功能所依赖的必要部分。

## 可由用户决定的后续选项

| 选项 | 收益与代价 | 建议 |
| --- | --- | --- |
| `status --json` | 可区分服务进程存活、配置加载失败和 Hook 数量；需要正确解释 `/status` 的 degraded 与 HTTP 200 | 若需脚本监控再加 |
| `hooks enable/disable` | 临时停用更方便；永久禁用涉及 Hook schema 与运行时语义 | 不顺带加入，确认需求后另评估 |
| macOS 服务管理 | 覆盖更多部署平台；需单独实现与实机测试 | 由实际部署平台决定优先级 |
| Agent 受控重连（P8） | 不重启整个进程即可应用身份、凭据和监听变化；需要连接生命周期、注册确认和失败恢复 | 建议在 P7 稳定后另行决定，首轮保留保存后人工重启 |

当前建议范围为 P1–P7：本地上下文 test/explain、主动刷新、init、Linux/Windows 服务，以及有限的 Hub/Agent 配置管理。P8 和表中剩余选项不自动进入实施范围。当前已实现 P1 的本地 hooks 查询与严格校验、P2 离线增删改及 P4 本地 test/explain；P3 的本地刷新和 init 已实现；P5 的 Linux systemd 代码已实现，真实环境验收待完成；P6/P7 及 P8 候选尚未实现，未安装任何系统服务。
