# tunlr CLI 设计文档

本文记录 tunlr CLI 的定位、边界、技术选型和关键设计决策，以及每个决策背后的理由。日常编码约定见仓库根目录的 `CLAUDE.md`。

## 1. 背景

起点是对现有工具的不满：

- **clashctl**（sh 脚本）：geo 资源更新有问题、没有 tproxy 配置、订阅不能自动更新；大量代码耗在适配不同 shell、init 系统和发行版上。
- **ShellCrash**：功能全，但为了覆盖路由器和各种固件，体积和复杂度都很大。
- **Mihari**：把太多功能放进了 TUI，并且依赖常驻守护进程。
- **clashtui**：同时适配 sing-box，范围超出需要。

结论：做一个只服务于“mihomo + 标准 Linux 服务器”的精简运维工具，把运行时交互交给已有的 Web 面板。

## 2. 品牌与命名

- 品牌：**tunlr**（tun + -lr），域名 `tunlr.app`，读作 “TUN-ler”。
- GitHub 组织：`tunlr-app`（`tunlr` 已被占用）；本仓库：`tunlr-app/cli`；命令：`tunlr`。
- tunlr 是一个产品方向，CLI 是第一个组件。将来可能有桌面客户端、手机 App、Web 控制台和托管服务，各自独立建仓库（如 `tunlr-app/desktop`、`tunlr-app/console`），官网放在 `tunlr-app/website`。
- mihomo 上游要求与 MetaCubeX 无关的下游项目名称中不得包含 “mihomo”。因此所有名称都不使用该词；README 中声明本项目与 MetaCubeX 无关，仅在描述用途时提及 mihomo。
- 历史提示：曾有一个叫 Tunlr 的 Smart DNS 服务（已停止运营），搜索结果会受影响；正式推广前建议查询商标。

## 3. 定位与边界

一句话：**mihomo 在 Linux 服务器上的运维层**。工具负责磁盘上的文件和系统状态，zashboard 负责运行中的内核。

### 3.1 交给 zashboard（工具不做）

- 切换节点和策略组、测延迟
- 查看和关闭连接、规则命中、实时日志、流量图
- 临时切换模式（rule/global/direct）
- 手动刷新 provider、清空 fake-ip 和 DNS 缓存

### 3.2 工具负责

- 内核安装、升级、回退与 checksum 校验
- systemd 托管、开机自启、定时任务
- 配置编排（模板 + 订阅 + mixin）、校验、原子发布、回滚、历史
- 透明代理开关及其系统前置条件（sysctl、防火墙、DNS 端口）和防失联回滚
- 面板部署与访问安全
- 诊断、救援、备份还原、当前 shell 代理变量

### 3.3 灰色地带的约定

原则：**不限制面板能力，工具做兜底**。内核支持的操作，面板照样能用。

- **TUN 开关**：面板可以在运行时切换，但不会写回配置文件，也不处理系统前置条件。持久化的开关以工具为准；工具读取实际状态，并提供“把当前运行状态保存到配置”的操作。
- **内核升级**：面板可以通过内核自带接口升级；工具负责兜底——保留已知可用版本，内核启动失败时自动回滚，并在定时任务中核对面板升级来的二进制（见第 7 节）。工具自己升级时会先做兼容性校验，没有断网窗口。
- **重载配置**：面板重载读的是工具生成的文件，不冲突。
- **geo 更新**：由内核按配置自己更新（面板手动刷新同样可以），工具的 `_update` 在更新后按官方 sha256 核对结果（见第 7 节）。
- **订阅更新**：订阅是完整配置，由工具的 timer（`tunlr _update`）拉取并走完整的配置流水线；配置中的 rule-providers 等仍由内核按 `interval` 刷新。

### 3.4 明确不做

- 全屏 TUI（进入备用屏幕的界面）、常驻守护进程（v1）
- sing-box 等其他内核、路由器固件、OpenRC/procd、WSL、iptables、容器化部署
- Telegram 机器人、公网入站、内网穿透、DDNS
- 失败通知（webhook 等，暂不做）：问题由 `doctor` 和 `status` 报告；需要时再议
- 在命令行里逐条增删节点和规则（编辑 mixin 即可）
- 遥测、账号体系、云同步

## 4. 支持矩阵

| 项目 | 支持 | 不支持 |
|---|---|---|
| 系统 | 使用 systemd 的 Linux（Debian 12+、Ubuntu 22.04+ 等） | OpenRC、procd、路由器固件、WSL |
| 架构 | amd64、arm64 | armv7、mips 等 |
| 防火墙 | nftables | iptables、混用环境 |
| 权限 | 工具以 root 运行 | 非 root 安装 |
| 内核 | mihomo 最新稳定版 | alpha 版、其他内核 |
| 面板 | zashboard（默认），metacubexd 可选 | — |

`doctor` 在安装时检查这些条件，不满足就拒绝并说明原因。架构上把 `service` 设计成接口（systemd 为第一个实现），为将来可能的 procd 支持留出扩展点，但 v1 不实现。

### 4.1 诊断（doctor，MVP 4）

参考 flutter doctor（分组列出）、brew doctor（只读，给出修复命令）、kubeadm 的 preflight（安装前复用同一套检查）。

- **定位**：`status` 回答“现在是什么状态”，要快；`doctor` 回答“为什么不正常、这台机器能不能用”，可以慢一些、更彻底。doctor 只读、不修复，每个问题给一条修复建议（多数是 `tunlr init`、`tunlr core start` 等已有命令：`init` 本来就是修复）。定时任务不调用它。
- **四组检查**：

  | 组 | 检查 | 何时 |
  |---|---|---|
  | `system` 支持条件 | Linux、root、PID 1 为 systemd、amd64 或 arm64（amd64 报告 CPU 级别，低于 v3 警告）、不在 WSL 中、不在容器中、发行版版本 | 总是。`init` 复用：有 fail 就拒绝安装 |
  | `tun` 透明代理条件 | `/dev/net/tun`、`nft`、`ip`、需要的 ip rule 优先级没有被占用、UFW 和 firewalld、iptables-legacy 中的规则 | 总是。`tun on` 复用：有 fail 就不开启，warn 照常显示 |
  | `install` 安装是否健康 | unit 与当前版本的模板一致；`/usr/local/bin/tunlr` 与正在运行的这个相同；内核用户存在，`bin/`、`core/` 归它所有；内核服务已启用、在运行，没有 failed 或反复重启；内核二进制存在且是已知可用版本；设置和状态文件能读取；生效配置存在并通过 `mihomo -t`；两个 timer 与设置和 TUN 开关一致，没有遗留的回滚定时器；TUN 开关与内核实际状态一致，内核在运行时两张 nft 表都在；配置中的端口没有被其他程序占用；订阅最近一次抓取是否失败、是否已到期或 7 天内到期、流量是否用完或超过 90% | 已安装（有 `tunlr-core.service`）时。没有安装时只有一项 skip，提示 `tunlr init` |
  | `network` 网络 | 能解析域名；能访问 api.github.com（校验值的来源）；配置了镜像时镜像能连上；本机时钟与 GitHub 的偏差（超过 90 秒警告，VMess 等协议会失败；超过 1 天判失败，TLS 会失败）；内核在运行时经 mixed-port 访问一个测试地址 | 默认执行，并行进行，总时长约 10 秒；`--offline` 跳过 |

- **分级**：ok、warn、fail、skip。有任何 fail 时退出码 3；只有 warn 时为 0。
  - WSL、容器判 fail（第 3.4 节明确不做），`init` 直接拒绝，不提供跳过检查的参数：不做兜底兼容。
  - 发行版不在 Debian 12+、Ubuntu 22.04+ 之列时判 warn，只要是 systemd 系统就允许安装。
  - iptables-legacy 中有规则时判 warn：只影响透明代理，不拒绝安装。
  - 订阅：启用的订阅已到期或流量用完判 fail，其余订阅判 warn。
- **非 root 运行**：照常运行，root 一项 fail，需要 root 才能查的项为 skip，用户仍能看到完整的报告。
- **输出**：按组输出，每项一行；warn、fail 下一行给出修复建议。`--json` 中每项有固定的 `id`（如 `system.wsl`、`install.units`），供脚本和将来的组件判断。
- **`status` 的提示**：`status` 末尾执行一次快速检查（不联网、不调用 `mihomo -t`），有 fail 时提示“N problems found; run tunlr doctor”。只看 fail：CPU 级别、UFW 等长期存在的 warn 每次都提示只会让人忽略。
- **菜单**：第一级加“Run diagnostics”（分类之后、退出之前），未安装时的菜单也有。

## 5. 技术选型

### 5.1 为什么是 Go 单文件，而不是 sh

| | 纯 sh | Go 单文件 |
|---|---|---|
| 依赖 | curl、yq、jq、shell 方言 | 只依赖 `nft` 和 systemd |
| 配置处理 | 文本替换，易出错 | 结构化 YAML 节点 |
| 错误处理与回滚 | 难写对 | 事务模式 + 单元测试 |
| 体积 | 几百 KB | 约 8–12 MB（去调试信息后，待实测） |

sh 的优势（轻量、随处可跑、可随手修改）主要服务于路由器和老旧固件，而本项目已经放弃这些平台。sh 只保留在安装脚本（识别架构、下载、校验后调用 `tunlr` 完成安装）。当前 shell 的代理变量不用 shell 函数：子进程无法修改父 shell 的环境变量，`tunlr proxy env` 只输出语句，由用户 `eval`（第 10 节）。

减小体积：`-ldflags="-s -w"`、`-trimpath`，另提供可选的 upx 压缩版本。也评估过 Rust（可小约 5 MB），但相对 17–19 MB 的内核压缩包意义不大，且 Go 的 `yaml.Node` 对“保留顺序和注释的合并”支持更好，生态也与 mihomo 一致。

### 5.2 为什么不做全屏 TUI

逐项核对功能清单后，没有任何一项必须用全屏界面：安装向导、确认、多选都能用 `huh` 在普通终端完成；状态用 `lipgloss` 渲染，配合 `--watch` 刷新；实时日志和流量交给 zashboard 或 `journalctl -f`。全屏界面还有几个具体的坏处：

- tunlr 的操作大多是一次性的事务（预览 diff、校验、发布、回滚、看日志），输出需要往回翻、复制、贴给别人；备用屏幕退出后内容就没了，经 SSH 复制也别扭。
- 全屏最擅长的是持续刷新的实时数据，那是 zashboard 的职责。
- 出问题时内联输出至少停在最后一行，看得出卡在哪一步；全屏界面会冻在半个画面上。VM 控制台、串口、窄窗口里全屏渲染也更容易乱。
- vim、less 和命令输出都要在全屏与普通屏幕之间来回交接。

所以约束是：不进入备用屏幕，所有输出留在滚动记录里。在这个前提下允许在 `internal/cli` 写 Bubble Tea 模型，但只用于展示与交互（菜单外壳、进度视图等），业务逻辑仍在 `pkg/core`；提问优先用 huh 的现成字段；每个交互功能都有等价的命令行参数；不做 zashboard 已有的实时视图。

### 5.3 为什么不常驻

| 需求 | 实现 |
|---|---|
| 定时更新 | `tunlr-update.timer` 每小时唤醒 `tunlr _update`，由它按设置的周期执行到期的更新（见第 7 节“定时任务的治理”） |
| 内核崩溃后恢复与回滚 | unit 中配置 `OnFailure=`，触发 `tunlr _rescue` |
| 开启 TUN 后的防失联回滚 | `systemd-run --on-active=<时长>` 创建临时回滚定时器 |
| 透明代理的规则丢失检测 | TUN 开启期间 `tunlr-check.timer` 每 5 分钟调用 `tunlr _check` |
| 状态与漂移 | 执行 `status` 时现场计算 |

好处：零常驻内存、更少的故障点、升级工具只需替换文件、日志由 journald 按 unit 归档。

### 5.4 依赖清单

见 `CLAUDE.md`。补充两点：

- **YAML 合并基于 `yaml.Node`**：保留键顺序和注释，生成的配置可读、diff 干净；不需要为 mihomo 的大量字段定义结构体。
- **nftables 用 `nft -f`，不用 `google/nftables`**：规则用 nft 原生语法写成模板，生成的脚本落盘可读、可手动执行、易于 golden 测试，`config plan --full` 原样给用户审查；`nft -f` 整个文件在一个事务中提交。用库直接发 netlink 的话，规则是一串字节表达式，与 `nft list ruleset` 对不上，用户无法审查，出问题也无法手动复现；很多语法特性还要自己拼表达式。`nft` 命令在目标系统上一定存在（`doctor` 要求 nftables），用户排查也离不开它。目前只需要加载、删表、列出表名，都用 `nft` 命令完成；将来真要程序化读取规则的计数器、集合内容等，再考虑引入它。
- **diff 自己实现**：锚定（patience）逐行 diff，约 80 行。原先用的 `sergi/go-diff` 是字符级的 diff-match-patch，按行用要把每行映射成一个字符，求的是最短差异，新旧差别大时要算几秒，统一格式也要自己拼；锚定算法以两侧各只出现一次的行对齐，耗时 O(n log n)（5000 节点换成 1 万节点约 30 毫秒），对每行多半唯一的配置结果也更易读。
- **正则用 `dlclark/regexp2`**（MIT，独立的库，不属于 mihomo）：mihomo 筛选节点用的就是它，节点筛选的正则要原样写给内核，两边必须一致（见第 6 节“节点筛选”）。
- **没有引入的**：`coder/websocket`（实时视图交给 zashboard，见 3.1 和 5.2）。

### 5.5 磁盘布局

| 路径 | 内容 | 说明 |
|---|---|---|
| `/etc/tunlr/` | mixin（`mixin.d/*.yaml`，订阅 mixin 在 `mixin.d/<订阅名>/*.yaml`，目录 0700、文件 0600）、工具设置（`settings.yaml`） | 用户可编辑，备份时最需要保留 |
| `/var/lib/tunlr/state.json` | 状态文件：结构版本号、安装实例 ID、订阅、已知可用内核、TUN 开关、网关开关、各定时更新最近一次的执行情况 | 工具维护，权限 0600。只存最新状态，不存会增长的历史（配置历史在 `history/`，历次更新在 journal） |
| `/var/lib/tunlr/subscriptions/` | 每个订阅最近一次成功抓取的内容 | 含机场凭据 |
| `/var/lib/tunlr/backups/` | `backup restore` 前自动备份的当前内容（`before-restore-<时间>.tar.gz`） | 保留最近 3 份，目录 0700、文件 0600 |
| `/var/lib/tunlr/history/` | 已发布配置的历史：每个修订一份配置（`<号>.yaml`）和一份记录（`<号>.json`） | 用于回滚，保留最近 10 个，权限 0600 |
| `/var/lib/tunlr/kernels/<版本>/mihomo` | 已知可用的内核 | root 所有，保留最近 3 个，见第 7 节 |
| `/var/lib/tunlr/bin/mihomo` | 运行中的内核，也用于校验配置 | 归内核用户 `tunlr` 所有，面板升级时原地替换 |
| `/var/lib/tunlr/core/` | mihomo 工作目录（`-d`）：生效的配置、API socket（`api.sock`）、geo 数据、面板、缓存 | 归内核用户 `tunlr` 所有，权限 0750（保护 API socket） |
| `/etc/systemd/system/tunlr-*.{service,timer}` | 工具生成的 unit：`tunlr-core.service`、`tunlr-rescue.service`、`tunlr-update.service`、`tunlr-update.timer`，MVP 3 起还有 `tunlr-check.service`、`tunlr-check.timer` | 每次 `init` 重写 |
| `/usr/local/bin/tunlr` | 工具自身 | unit 引用这个路径，`init` 时不在这里则复制过来 |
| `/etc/sysctl.d/90-tunlr-gateway.conf` | 网关模式的 `net.ipv4.ip_forward = 1`（MVP 5，见 8.10） | 只在网关开启时存在，`tun gateway off`、`uninstall` 时删除 |
| `/run/lock/tunlr.lock` | 文件锁 | 放在 `/run` 下，安装前也可用 |

- `/var/lib/tunlr` 为 0711：内核用户只能进入它拥有的 `bin/`、`core/`，不能列出或读取其他内容（订阅、历史、状态文件都是 root 的 0600/0700）。

- 所有路径由 `pkg/core/paths.Layout` 统一给出，测试时设置 `Root` 把整套布局放进临时目录。这样的测试布局不涉及内核用户：文件不改属主，`mihomo -t` 以当前身份运行。
- 文件一律经 `pkg/core/fsutil.WriteFile` 原子写入：同目录临时文件 → 刷盘 → `os.Rename` → 刷新目录。
- 锁被占用时重试到期限为止，超时报告“另一个 tunlr 操作正在进行”。锁文件不删除，否则并发的获取者会锁住不同的文件。
- 状态文件读取时检查版本：比当前工具新则拒绝并提示升级，比当前旧则按迁移链逐级升级。
- 工具设置（`pkg/core/settings`）由用户编辑，文件不存在时用默认值；未知的键视为错误，避免拼写错误被静默忽略。目前的设置项：`github-mirror`（GitHub 下载镜像前缀，只支持一个；作用见第 6 节“内核自己的下载”）、`user-agent`（抓取订阅的默认 User-Agent）。

## 6. 配置流水线

```
模板 + 订阅（机场完整配置）+ 受管 mixin（tunlr 的交互功能）+ 用户 mixin
        → 合成最终配置（用户 mixin 动了受管的键时拒绝）
        → mihomo -t 校验
        → 写临时文件 → os.Rename 原子替换
        → 通过 API 热重载
        → 失败则恢复上一版本
        → 记录历史（保留最近 N 份）
```

- **订阅是机场提供的完整配置**（含节点、策略组和规则）。实际使用中机场的价值主要在它维护的策略组和分流规则，只取节点再由工具模板提供规则不符合需求。因此工具自己抓取订阅，抓取失败时保留上一次成功的结果。
- **可以保存多个订阅，同一时间只启用一个**。订阅本身就是一份完整的 YAML，不存在多份订阅的节点、策略组和规则互相合并的情况。下面的分层合成只是在启用的那份订阅之上叠加系统设置和用户覆盖。
  - 适用场景：有备用机场、或在几个机场之间切换。`sub use <name>` 切换启用的订阅，切换本身就是一次配置发布，同样经过校验，失败则回滚。
  - 每个订阅保留最近一次成功抓取的内容。切换到从未成功抓取的订阅时先抓取，失败则不切换。
  - `mixin.d/*.yaml` 对所有订阅共用，每个订阅另有自己的 mixin（`mixin.d/<订阅名>/*.yaml`，只在它启用时应用，排在共用的之后，见下文“订阅 mixin”）。共用的 mixin 可能引用某个机场特有的策略组：`patch`、`delete` 找不到目标时只给出警告；插入了不存在的名称等导致 `mihomo -t` 不通过时，不发布并提示原因。
  - 不能删除正在启用的订阅，需先切换到其他订阅。
- **订阅抓取**（`pkg/core/subscription`）：
  - 机场按 User-Agent 决定返回的格式。默认 `clash.meta mihomo tunlr/<版本>`，同时命中面板常见的 `clash.meta` 与 `mihomo` 子串判断；返回 base64 节点列表时，报错提示更换 User-Agent。可以自定义，优先级从高到低：
    - 单个订阅自己的 User-Agent：`sub add --user-agent` 指定，已有订阅用 `sub update <name> --user-agent` 修改（只在抓取成功时保存，试错不会丢掉原来可用的值；`--user-agent ""` 恢复默认）。
    - `settings.yaml` 中的 `user-agent`，作用于所有没有自己 User-Agent 的订阅。
    - 内置默认值。
  - User-Agent 不能含控制字符或首尾空白，避免 HTTP 头注入。
  - 内容必须是 YAML 映射且带有节点（`proxies` 或 `proxy-providers`），否则视为抓取失败。响应限制 32 MB、60 秒。
  - 读取 `subscription-userinfo` 响应头，记录流量与到期时间，供 `sub` 显示和将来的到期提醒。
  - 订阅 URL 通常带有访问令牌：输出、日志和错误信息中只出现 `scheme://host/...`，net/http 错误中附带的完整 URL 会被去掉。
  - 订阅列表和抓取信息存在状态文件中，内容存在 `subscriptions/<名称>.yaml`（目录 0700，文件 0600）。添加失败时不留下任何文件；内容未变时不重写，减少刷盘（测试机的虚拟磁盘每次刷盘 35～140 毫秒）。
  - 只含节点的订阅（走 proxy-providers、由模板提供策略组和规则）留作将来的第二种来源，与完整配置共用同一条流水线。
- **订阅的来源**（2026-10-29）：地址、本地文件或标准输入，由 `sub add <名称> <来源>` 的第二个参数决定，不另设参数：含 `://` 的是地址（只认 http、https），`-` 是标准输入，其余都是路径（转为绝对路径，必须是普通文件）。
  - 本地文件记住路径（状态中的 `path`，与 `url` 二选一），`sub update` 时重新读取；内容照样复制到 `subscriptions/`，源文件删掉后配置照常可用，`sub update` 记一次失败，与抓取失败相同。自动更新默认关闭（文件只会被用户自己改，改完 `sub update`），可以用 `update set sub` 打开。
  - 标准输入用于文件在自己电脑上的情况：`ssh host tunlr sub add home - < airport.yaml`。没有可以重读的来源，不自动更新（`update set sub` 拒绝），`update run`、定时任务跳过它；更新时由 `sub update <名称> --source -` 再读一次标准输入。
  - `sub update <名称> --source <来源>` 换一个来源（地址、文件或 `-`），与 `--user-agent` 相同，只在读取成功时保存。换机场地址也不必再删了重加。
  - 本地文件和标准输入没有流量与到期信息，也没有 User-Agent（`--user-agent` 报用法错误）。内容的要求与地址相同。`sub` 的来源一栏显示路径或 `stdin`。
- **分层合成，各层职责固定**（`pkg/core/config.Compose`）：
  1. 模板：工具内置的系统部分，如端口、DNS、geo 数据的自动更新与下载地址、面板、profile。
  2. 订阅：**只取白名单中的键**（`config.subscriptionKeys`），其余键一律丢弃并在 `config plan` 中列出。订阅是不可信输入，不能让机场配置打开端口、改监听地址或设置 API。随后按 mixin 中的 `tunlr.proxy-filter` 筛选订阅的节点，见下文“节点筛选”。白名单：
     - 路由：`proxies`、`proxy-groups`、`rules`、`proxy-providers`、`rule-providers`、`sub-rules`。
     - DNS：`dns`、`hosts`、`sniffer`。机场的规则常与它的 DNS 配合（`nameserver-policy`、fake-ip 过滤、按 geosite 分流的上游），换成模板的 DNS 可能让规则失效，所以整段采用订阅的 `dns`，替换模板的。例外是 `dns.listen`：它决定 DNS 是否暴露在外（见下文“端口安全”），TUN 的 DNS 劫持也转到它，一律换回模板的 `127.0.0.1:1053`，在 `config plan` 中列为丢弃了 `dns.listen`。开启 TUN 时 `tunlr:tun` 独占 `dns.enable: true`：TUN 劫持 DNS，内核的 DNS 必须开着。
     - geo 数据：`geox-url`、`geodata-mode`、`geodata-loader`、`geo-auto-update`、`geo-update-interval`。订阅的规则要用它指定来源的数据（如自己维护的 geosite 分类），见第 7 节“geo 数据更新”。
     - 订阅的 DNS 是为桌面客户端写的，可能在服务器上不成立。合成后检查（`config.DNSWarnings`），作为警告列在 `config plan` 中、不阻止发布：开启 DNS 却没有 `nameserver`；`dhcp://<网卡>` 指向本机没有的网卡（如 macOS 的 `en0`）。要改用 mixin 覆盖 `dns` 下的键。
  3. 提供默认值的受管 mixin（`tunlr:mirror`），见下文“受管 mixin”。
  4. 用户的 mixin：先是共用的 `mixin.d/*.yaml`，再是启用订阅的 `mixin.d/<订阅名>/*.yaml`，见下文。
  5. 独占键的受管 mixin（`tunlr:core`、`tunlr:tun`、`tunlr:panel`、`tunlr:proxy`），最后写入。
  6. 设置了 `github-mirror` 时，`geox-url` 中 `https://github.com/` 开头的地址改为经镜像（`Layers.Mirror`），不论来自模板、订阅还是 mixin：这样订阅自定义的来源也能在国内服务器上下载，工具的核对（见第 7 节）去掉镜像前缀后仍认得出是哪个 GitHub release 资产。其他地址原样保留。
- **受管 mixin：mixin 是统一的底座**。mihomo 由配置驱动，tunlr 的交互功能（TUN 开关、面板与代理端口的局域网访问等）说到底都是对配置的修改。每个功能的开关存在状态文件或工具设置中，它在配置中写成什么由一份受管 mixin 表达，与用户的 mixin 走同一套合并引擎（`pkg/core/config/managed.go`）。原来分开的“工具生成的调整”和“强制项”两条路径合并为它。
  - 两种受管 mixin：

    | 种类 | 成员 | 应用时机 | 用户的 mixin 动了它的键 |
    |---|---|---|---|
    | 独占 | `tunlr:core`（`external-controller-unix`，工具经它访问 API）<br>`tunlr:tun`（`tun.enable` 等，见 8.3）<br>`tunlr:panel`（`external-controller`、`secret`）<br>`tunlr:proxy`（`allow-lan`、`lan-allowed-ips`，见第 10 节“代理端口的局域网访问”） | 用户的 mixin 之后 | 报错，不发布 |
    | 默认值 | `tunlr:mirror`（配置了 `github-mirror` 时：`external-ui-url` 和让镜像直连的规则；`geox-url` 不在这里，见上面第 6 步） | 用户的 mixin 之前 | 允许，`config plan` 中记为“覆盖 tunlr:mirror” |

  - 冲突：用户的 mixin 在 `mihomo:` 下写了独占的键（即使值相同）、用 `null` 删除它，或把它的上级整个替换掉。报告为 mixin 错误（`*config.MixinError`，退出码 3），指出文件、行和改用的命令，例如 `mixin 20-x.yaml:3: tun.enable is managed by tunlr:tun (change it with: tunlr tun on/off); remove it from the mixin`。同一映射中的其他键照常可改（如 `tun.stack`、`tun.route-exclude-address`）。
  - 为什么拒绝而不是像原来那样忽略并警告：被忽略的设置让人以为生效了，交互命令和手改的 mixin 各说各的；拒绝时用户立刻知道该用哪个命令。代价是工具升级新增独占的键后，已经写了它的 mixin 会让定时更新保持当前配置（与 mixin 出错相同），`doctor` 的 `install.mixins` 列出这种冲突。
  - `secret` 也是独占的，不支持自定义：换 secret 用 `tunlr panel reset-secret`。
  - 受管 mixin 只在内存中合成，不写入 `mixin.d`：写成文件会让用户以为可以编辑它，改了又被工具覆盖；设置变化时还要同步文件。`tunlr config` 列出用户的 mixin 文件和全部受管 mixin（独占的列出键和命令，默认值的列出内容，`secret` 的值不出现），`config plan` 列出受管 mixin 的名称和默认值的内容。
  - 默认值放在订阅之后而不是紧接模板：插入规则需要订阅的 `rules` 已经就位；这一层的键模板中都有，合并后位置不变，输出稳定。
  - 回滚和自动关闭透明代理不重新合成，只把独占的受管 mixin 按当前状态重新写入生效的配置（`config.Enforce`），包括面板的监听地址和 `secret`。
- **mixin**（`pkg/core/config/mixin.go`）。参考了 clashctl（单个 mixin.yaml，嵌套的 `prepend`/`append`/`override`/`inject`）和 ShellCrash（按类别分文件、文本拼接、`-t` 失败时静默丢弃全部自定义）后的设计：
  - 场景：改通用设置；加自己的节点、策略组、规则、rule-providers、listeners；改订阅里已有的策略组和节点（塞入自己的节点、改 interval、加 dialer-proxy）；去掉机场的信息节点和不要的组或规则；按用途分文件、可以单独停用。
  - **两个命名空间：`mihomo:` 是原生配置，`tunlr:` 是 tunlr 的语法**（2026-10-29 讨论后定）。一个 mixin 文件的顶层只有这两个键（外加放锚点的 `x-`），各自可以省略：

    ```yaml
    mihomo:                      # 原样合并进生成的配置，不含任何 tunlr 的写法
      log-level: warning
      dns:
        nameserver: [192.168.1.1, 223.5.5.5]
    tunlr:                       # 由 tunlr 解析，不进入配置
      proxy-filter:
        exclude-filter: 倍率
      proxies:
        append:
          - {name: 家宽, type: socks5, server: 203.0.113.1, port: 1080}
      proxy-groups:
        patch:
          - name: 节点选择
            prepend-proxies: [家宽]
      rules:
        prepend:
          - DOMAIN-SUFFIX,corp.example.com,家宽
        delete:
          - /^GEOIP,CN/
    ```

    - 为什么分开：mixin 里有两类东西，mihomo 的配置和只有 tunlr 认识的操作。混在一层里，看一个键要先判断它是谁的。分成两块后，`mihomo:` 下的每个键都能直接查 mihomo 文档，`tunlr:` 下的每个键都在本节有定义，谁来解析一目了然，两边各自可以独立演进。
    - 为什么不做成只有 `tunlr:` 的一套语言：mihomo 的配置面很大且一直在加，建模一部分就有改不了的东西，全部建模等于永远追着上游重写 schema，mihomo 的文档和示例也用不上了。`mihomo:` 就是这个兜底：tunlr 没覆盖到的，照样能改。
    - 为什么操作只针对几个列表：tunlr 的操作只对订阅带来的三个列表（节点、策略组、规则）有意义，它们是 mihomo 配置里仅有的“用户必须在机场的内容上增删改”的地方；其余键都来自模板或用户自己，整体写一遍就够。所以 `tunlr:` 下按这几个对象分组，而不是对任意一层都提供增删改。
    - 顶层出现这两个键（和 `x-`）之外的键时报错，不猜测用户的意图。

  - **`mihomo:` 的合并语义**：与 mihomo 配置完全一致的写法，按 RFC 7386 JSON Merge Patch 合并：

    | 写法 | 语义 |
    |---|---|
    | `键: 值` | 映射逐层合并，标量和列表整体替换。只写 `dns.nameserver` 不会清掉 `dns` 下的其他字段 |
    | `键: null` | 删除该键 |

    - 列表只整体替换，不提供插入和删除：要在列表里增删的只有订阅带来的三个列表，归 `tunlr:`。嵌套的列表（`dns.nameserver`、`listeners`、`tun.route-exclude-address` 等）都来自模板或用户自己，整体写一遍是最清楚的写法。
    - 在这里整体替换 `proxies`、`proxy-groups`、`rules`、`sub-rules` 时给出警告，指向 `tunlr:` 下的操作（订阅更新后机场的内容会全部丢掉，多半不是本意）。
    - 受管的键（见上文“受管 mixin”）在这里和在 `tunlr:` 里一样不能动。

  - **`tunlr:` 的操作**：下面的键之外一律报错，免得写错了以为生效。

    | 键 | 含义 |
    |---|---|
    | `proxy-filter` | 筛选订阅的节点，见下文“节点筛选” |
    | `proxies`、`proxy-groups`、`rules` | 对订阅的同名列表做操作，各自是一个映射，键为下表的四种操作 |
    | `sub-rules` | 映射：`<名称>` → 同样的四种操作，作用于 `sub-rules.<名称>` |

    | 操作 | 语义 |
    |---|---|
    | `prepend` / `append` | 在列表前插入 / 后追加，列表不存在时创建。规则列表（`rules`、`sub-rules.<名称>`）追加在末尾的 `MATCH` 之前，否则追加的规则永远不会命中 |
    | `patch` | 每一项按 `name` 找到列表中的项，按 `mihomo:` 的语义递归修改它（映射合并、标量和列表替换、`null` 删除字段）。项里对列表字段的增删用 `prepend-<字段>`、`append-<字段>`、`delete-<字段>`，如策略组的 `prepend-proxies`：这是前缀写法仅剩的位置，显式写出，不靠值的类型猜。找不到时警告，不报错：机场改名很常见 |
    | `delete` | 按 `name`（规则按整条内容）删除。删除节点或策略组时，同时从所有策略组的 `proxies` 中移除 |

    - `patch`、`delete` 中的名称写成 `/正则/` 时匹配所有符合的项（用于批量修改同一地区的节点、删掉个别的组或规则；成批筛选节点用 `proxy-filter`）。
    - 操作的值必须是列表（`null` 视为没写）。
    - 不给 `proxy-providers`、`rule-providers` 设操作：它们是映射，在 `mihomo:` 里逐键合并、`null` 删除已经够用。有真实需求再加。
  - **一个文件内的执行顺序**固定，与书写顺序无关：先 `mihomo:`（替换与合并），再 `tunlr:` 的 删除 → 修改 → 插入。这样先删掉订阅的 `MATCH` 再追加自己的 `MATCH`、删除信息节点不会误删自己追加的节点；`patch` 也能改到 `mihomo:` 里刚替换的内容。`proxy-filter` 不在这个顺序里：它在任何 mixin 之前执行（见下文）。
  - 顶层以 `x-` 开头的键不写入配置，用来放 YAML 锚点；锚点不能跨文件。mixin 中的注释不带入生成的配置。
  - 多个文件时两个命名空间都是“后面的文件作用在前面的结果上”：`mihomo:` 逐键覆盖，`tunlr:` 的操作逐个执行（后面文件前插的规则排得更靠前，后面的文件可以 `patch` 前面文件加入的组），`proxy-filter` 逐字段覆盖。
  - 受管 mixin 在内部用同一种写法表达（独占的键写在 `mihomo:` 下，`tunlr:mirror`、`tunlr:filter` 插入的规则写在 `tunlr.rules.prepend` 下），走同一套引擎。
  - **正则与 mihomo 相同**（2026-10-29）：所有正则（`/正则/` 和 `proxy-filter`）都用 `github.com/dlclark/regexp2`，即 mihomo 筛选节点所用的库（.NET 语法，支持前后查找）。同一个字符串既由 tunlr 执行、又原样写给内核，两边的结果必须一致；只有一种正则语义，也不必记哪里支持什么。原来用 Go 的 `regexp`（RE2），常见写法两者相同。
- **节点筛选（`tunlr.proxy-filter`）**（2026-10-29）：mihomo 没有全局的节点筛选，只有 provider 和策略组上的 `filter`、`exclude-filter`、`exclude-type`，而且策略组上的 `filter` 对 `proxies:` 中按名称列出的节点不生效。机场的完整配置绝大多数是顶层列出节点、策略组按名称引用，原生的筛选一个也去不掉，所以由 tunlr 实现：

    ```yaml
    tunlr:
      proxy-filter:
        filter: "(?i)港|hk`日本"        # 只保留匹配的节点，多条用反引号分隔，任一匹配即可
        exclude-filter: "倍率`剩余流量"   # 去掉匹配的节点
        exclude-type: "ss|http"         # 按配置中的 type 去掉，| 分隔，不区分大小写
    ```

  - 字段、写法和语义照搬 mihomo 的 provider（读源码 `adapter/outboundgroup/groupbase.go` 与文档）：先取 `filter`，再去掉 `exclude-filter`，最后去掉 `exclude-type`，两边都匹配时去掉。按 provider 而不是策略组的语义，因为它决定的是“有哪些节点”：`exclude-type` 匹配配置中 `type` 的写法（`ss`、`vmess`），而不是策略组用的内部类型名（`Shadowsocks`）。
  - 写在共用的 mixin 里对所有订阅生效，写在订阅 mixin 里只对这个订阅生效、并逐字段覆盖共用的。
  - **只筛选订阅带来的节点**：在订阅层之后、任何 mixin 之前执行（用的是全部 mixin 合并后的 `tunlr.proxy-filter`）。mixin 用 `tunlr.proxies.append` 加的自己的节点不受影响，`tunlr.proxies.delete` 照常只删点名的。
  - 订阅中直接列出的 `proxies`：tunlr 筛选，去掉的节点从所有策略组的 `proxies` 中移除。策略组因此变空（原来有成员，现在没有，且没有 `use`、`include-all*`）时删掉它，并从其他组中移除，直到没有空组；mihomo 运行时会给空组放一个兜底出站，但组里已经没有可用的节点，发布前删掉、在 plan 中说明更清楚。被删的组如果在全部 mixin 应用之后仍被规则（`rules`、`sub-rules`）引用，报错不发布（退出码 3），列出这些规则，由用户放宽筛选或用 mixin 处理，不悄悄把这部分流量改成直连或拒绝。订阅只有直接列出的节点、却全被筛掉时同样报错。
  - 订阅中的 `proxy-providers`：tunlr 看不到其中的节点，把三个字段写进每个 provider 由内核筛选。provider 原来就有的：`exclude-filter` 用反引号、`exclude-type` 用 `|` 追加（都是任一匹配即去掉，可以直接拼接）；`filter` 要两边都满足，组合为 `^(?=.*?(?:原有))(?=.*?(?:新的))`（多条时各自先合成 `(?:a)|(?:b)`），只有 regexp2 支持前瞻，这也是正则要与 mihomo 一致的原因之一。
  - 正则在合成时检查语法。订阅只有直接列出的节点时，某条正则一个节点都没匹配到给出警告（多半是写错了）。`config plan` 给出一行摘要，如 `Proxy filter: kept 20 of 43 proxies; removed empty groups: 美国, 新加坡; filtered by mihomo in providers: airport`，`--json` 中为 `proxy_filter`。
- **多个 mixin 文件**：`/etc/tunlr/mixin.d/*.yaml` 按文件名排序，**逐个应用**，每个文件作用在前一个文件处理后的结果上。由此后面的文件优先级一致地更高：同一个键以后面的为准（plan 列出“哪个文件覆盖了哪个文件”），后面文件前插的规则排得更靠前，后面的文件可以 patch 前面文件加入的组。
  - 分类由文件名约定，工具不强制，例如 `10-dns.yaml`、`20-nodes.yaml`、`50-rules.yaml`。只读取 `.yaml`，改名为 `.yaml.off` 即停用；以点开头的文件（编辑器临时文件）被忽略。
  - 排序按文件名逐字符比较，与 `ls`、`run-parts`、systemd 的 drop-in 目录一致，不做“自然排序”（按数值比较数字前缀）：规则简单，用户看到的 `ls` 顺序就是应用顺序。代价是位数不同的前缀会出乎意料（`9-a.yaml` 排在 `10-b.yaml` 之后），因此 `config plan` 发现数字前缀的数值顺序与应用顺序不一致时给出警告，并建议补零后的文件名；`tunlr config edit` 新建文件时列出已有文件的应用顺序，方便选择前缀。
  - **订阅 mixin**（2026-10-29）：`mixin.d/<订阅名>/*.yaml`，写法与共用的相同，排在共用的之后，所以覆盖共用的（plan 照样列出覆盖关系）；只应用启用的那个订阅的，`sub use` 切换时 diff 中可以看到两边的 mixin 互换。用途是只对一个机场有意义的修改：它的节点筛选、它特有的策略组。文件名在提示中写成 `<订阅名>/<文件名>`，`config edit home/10-filter` 编辑它。
    - `sub add` 的 `--include`、`--exclude`（可重复，多个用反引号连起来）只是快捷方式：添加成功后写入 `mixin.d/<名称>/10-filter.yaml`（`tunlr.proxy-filter` 的 `filter`、`exclude-filter`），之后用 `config edit <名称>/10-filter` 修改。不另设 `sub filter`：筛选就是 mixin 的一种能力，改它与改其他 mixin 是同一条路。
    - 跟着订阅走：`sub remove` 一并删除（确认时列出文件数），备份与还原包含它；没有这个订阅的目录被忽略，`doctor` 不报错。更深的子目录被忽略。
    - 数字前缀的顺序警告在共用的和每个订阅的文件之间分别检查。
    - 编辑没有启用的订阅的 mixin 时，按“它被启用”合成并运行 `-t` 来检查，通过后保存，不发布，提示 `sub use` 后生效。
  - mixin 可能含有节点密码，目录 0700，文件 0600。
- **范围与检查**：
  - 受管的键：动了独占的受管 mixin 的键时报错（见上文“受管 mixin”）。
  - 允许但警告：数字前缀位数不同导致的意外顺序；在 `mihomo:` 中整体替换订阅的 `rules`/`proxies`/`proxy-groups`/`sub-rules`；`patch`、`delete` 没有匹配的项。
  - 报错（退出码 3，不发布）：YAML 解析失败；顶层有 `mihomo`、`tunlr`、`x-` 之外的键（提示放到 `mihomo:` 下）；动了受管的键；`tunlr:` 下有不认识的键，或操作的值不是列表；`patch` 的项没有 `name`；正则无效；端口安全检查不通过（见下）；以及 `mihomo -t` 不通过。
  - **端口安全**（MVP 5）：合成的结果会让其他主机无需凭据就能使用代理或 DNS 时不发布，避免公网服务器成为开放代理、开放 DNS 解析器。由 `pkg/core/config.CheckExposure` 检查：
    - `allow-lan: true` 而 `authentication` 为空，且 `lan-allowed-ips` 不是只含私有地址段（每一项都在 `config.LANAllowedIPs` 的某个网段之内；没写这个键时内核放行所有来源）：代理端口（`mixed-port`、`port`、`socks-port` 等）对外开放且没有认证。`tunlr proxy lan on` 写的就是只含私有地址段的列表，所以通过。
    - `listeners` 中 `mixed`、`http`、`socks` 类型的项监听在非回环地址（不写 `listen` 即为 `0.0.0.0`），且没有认证：写了 `users: []`，或没写 `users`（这时沿用全局的 `authentication`）而全局也为空。
    - `dns.enable` 时 `dns.listen` 不在回环地址：在公网上会被用于 DNS 反射放大。局域网设备经网关模式（8.10）使用时由 TUN 劫持，不需要它。
    - `external-controller` 有值而 `secret` 为空（`secret` 归 `tunlr:panel`，mixin 改不了，只会出现在工具之外改动的配置中）。
    - 只检查重新合成的配置（`config plan`、`config apply`、`config edit`、`sub use`、`tun on/off`、定时更新）。回滚到历史修订、自动关闭 TUN 时只重写独占的键、不重新合成，不检查：这些是已经发布过的内容，拦下它们只会让恢复失败。定时更新遇到这种错误时保持当前配置，与 mixin 出错相同。
    - 认证的用户名和密码由用户写在 mixin 里，tunlr 不生成、不保存。`doctor` 对生效的配置做同样的检查（`install.exposure`）。
  - mixin 有错时绝不丢掉它去发布（ShellCrash 的做法）：丢掉用户写的直连或拦截规则可能让流量走错路。定时更新遇到 mixin 错误同样保持当前配置。
- **`tunlr config edit [名称]`**：同 visudo、`kubectl edit`，在临时副本上编辑，保存后合成并运行 `-t`，有错就显示原因并让用户选择重新编辑或放弃，**通过后才写回 `mixin.d`**，然后显示 diff、确认后发布。有错的编辑因此不会进入 `mixin.d`，定时任务读不到。
  - 新文件以带完整说明、全部注释掉的示例开始（`templates/mixin.yaml`，`mihomo:` 与 `tunlr:` 两段各给示例），未修改就不保存。省略名称时从已有文件中选择或新建（共用的，或启用订阅的）。名称写成 `<订阅名>/<文件名>` 时是订阅 mixin，订阅必须存在。
  - `-t` 因下载数据失败而未能完成时，可以选择只保存不发布。编辑期间文件被别人改过时不覆盖，编辑结果留在临时文件中。
  - 编辑器取 `$VISUAL`、`$EDITOR`，否则依次尝试 `vim`、`nano`、`vi`（有 vim 就用 vim，不经 Debian 的 `editor` 替代项，它常指向 nano）；需要终端，非交互模式下报用法错误，直接改文件后用 `config plan`/`config apply` 即可。
  - 打开编辑器前只说明 tunlr 自己的规则（保存退出后校验并显示差异、确认后才发布；不保存退出则不做改动），不讲编辑器本身的用法。
- 合成前展开订阅中的 YAML 锚点与别名（被引用的锚点可能位于被丢弃的键里），并限制展开后的节点数，防止恶意订阅放大。
- **校验前必须备好 geo 数据**。`mihomo -t` 会加载规则用到的 GeoIP/GeoSite 数据，缺失时现场从 GitHub 下载；在国内服务器上实测下载耗时 84 秒，超过 90 秒即判定校验失败，与配置本身无关。因此：
  - 工具从配置判断需要哪些 geo 数据（规则、DNS 设置中出现 geoip、geosite、ip-asn 即视为需要；宁可多备一个文件，不能漏），缺失的按生效的 `geox-url` 下载到内核工作目录：来源是 GitHub release 资产的按官方 sha256 校验（见第 7 节的镜像与校验），其他来源（订阅或 mixin 自定义的 CDN 等）原样下载、无从校验。已有的文件不在这里更新，由内核自己更新、`_update` 核对（见第 7 节）。预先备好后实测校验耗时 49 毫秒。
  - `-t` 在临时工作目录中运行，内核工作目录中的 geo 文件以符号链接提供：`-t` 可能写入缓存，直接用内核的工作目录会与运行中的内核冲突。
  - 校验失败时区分“配置错误”和“依赖下载失败”：mihomo 的错误消息含 `can't download` 时属于后者，不报告为配置问题（不映射到退出码 3）。
  - 实测 `-t` 不会抓取远程的 proxy-providers 和 rule-providers，它们暂时不可达不影响校验。
  - 内核自己的 geo 定时更新（`geox-url`）同样受网络影响：设置了镜像时，合成的最后把其中 github.com 的地址改为经镜像（见上文合成的第 6 步）。
- **内核自己的下载**：

  | 下载 | 发起者 | 路径 | 校验 |
  |---|---|---|---|
  | 内核、校验前补齐的 geo 数据、面板（首次） | tunlr | 直连或经镜像 | 官方 sha256 |
  | 订阅 | tunlr | 直连 | 无（机场内容无从校验） |
  | 面板手动更新、面板上手动更新 geo | 内核 | 按规则分流，地址经镜像 | 无 |
  | geo 自动更新（`geo-auto-update`）、`update run geo` | 内核 | 按规则分流，github.com 的地址经镜像 | 事后由 `_update` 按官方 sha256 核对（GitHub release 资产） |
  | 订阅中的 proxy-providers、rule-providers | 内核 | 按规则分流，或 provider 的 `proxy` 字段 | 无 |
  | 面板触发的内核升级 | 内核 | 按规则分流，直连 github.com | 无，事后由工具核对（见第 7 节） |

  - 读 mihomo 源码（`component/http`）确认：内核自己的 HTTP 请求交给内部 tunnel，按目标域名匹配规则，失败时才退回直连。所以策略组选中不可用的节点时，面板和 geo 下载全部失败（实测）。
  - **镜像直连规则**：配置了 `github-mirror` 时，工具在订阅规则前插入 `DOMAIN,<镜像域名>,DIRECT`（镜像是 IP 时用 `IP-CIDR`/`IP-CIDR6`）。镜像本来就是为直连准备的；实测选中不可用的节点时，加上这条规则后面板经镜像下载成功。它属于提供默认值的 `tunlr:mirror` 而不是独占的：用户有理由让镜像也走代理，可以用 `tunlr.rules.delete` 去掉。没有配置镜像时不加，直连 github.com 在国内很慢，交给规则决定。将来 TUN 阶段需要直连的主机（如订阅域名）也放在这一层。
  - **信任边界**：镜像可以篡改它转发的内容。工具的下载有官方 sha256 兜底，内核的下载没有。其中面板在浏览器中运行并持有 API secret，geo 数据决定分流，被篡改的后果都很严重，因此改由工具下载并校验：
    - 面板：apply 时若面板目录不存在，工具从 zashboard 最新发布下载 `dist.zip`，按官方 sha256 校验，解压（拒绝指向目录外的路径和非普通文件，限制解压后大小）后原子地放到位。实测内核启动时发现面板目录已存在就跳过下载。只处理 `external-ui-url` 仍是 zashboard 官方地址或其镜像地址的配置，用户在 mixin 中换了面板时交给内核。面板不是必需的：下载失败（时限 5 分钟）照常发布，由内核按 `external-ui-url` 自己下载；下载在加锁之前进行，不阻塞定时任务。
    - geo 数据：校验前缺失的文件由工具校验下载；之后由内核按配置自己更新，`_update` 在每次更新后按官方 sha256 核对，不一致的换成校验过的文件并重启内核（见第 7 节“geo 数据更新”）。来源不是 GitHub release 资产的无从校验，`update run geo` 的结果中注明。
    - `external-ui-url`、`geox-url` 仍写成经镜像的地址，用于内核自己的更新和用户在面板上主动点“更新”。
  - 实测：zashboard 与 meta-rules-dat 的发布资产都带有 GitHub 提供的 sha256 digest。
- **充分利用内核已有能力**，由工具写入模板默认开启：
  - `external-ui` / `external-ui-url`：面板的位置（`ui/zashboard`，挂在 `/ui`）和更新地址（首次由工具校验安装，之后可在面板上手动更新）
  - proxy-providers / rule-providers 的 `interval`：订阅与规则集热更新
  - `profile.store-selected` / `store-fake-ip`：保存面板选择的节点和 fake-ip 映射
  - `ntp`：时间同步（或交给系统的 timesyncd）
- 工具经 `external-controller-unix` 访问内核 API，路径由 `tunlr:core` 写为相对路径 `api.sock`。内核相对于工作目录解析它，所以在临时目录中运行的 `-t` 不受影响。实测内核以 0666 创建这个 socket，访问也不需要 secret，因此只能靠工作目录的权限（0750）阻止其他本地用户。每次发布都会把工作目录的权限收紧到 0750。面板走 `external-controller`（HTTP，只监听本机），由 secret 保护：实测不带 secret 时返回 401。
- **发布分两步：预览（plan）与发布（apply）**（`pkg/core/pipeline`）：
  - plan：合成配置，与生效的配置比较，生成统一格式的 diff，并用 `mihomo -t` 校验。plan 不改动状态，只可能补齐校验需要的 geo 数据。内核未安装时 plan 仍可预览，但标明未校验；apply 则拒绝发布。
  - apply 先输出 plan 的结果，经确认后在文件锁内发布：原子写入 → 通知内核加载 → 记录历史。
    - 确认等待期间不持有锁，避免阻塞 timer 触发的任务。代价是加锁后要核对生效的配置与预览时一致，否则拒绝发布并要求重新预览，不覆盖用户没看过的变化。
    - 非交互模式下必须带 `--yes`，否则在开始工作前报用法错误。`--dry-run` 只预览。
  - 退出码：校验未通过为 3。内核加载失败时恢复原配置、让内核重新加载并返回 4，失败的配置不进历史。
  - 生效的配置被手工改过（与最新修订不一致）时，发布前先把它存为一个来源为 found 的修订，以便找回。
  - 生效的配置不存在（如卸载后重新 `init`）而发布的内容与最新修订相同时，沿用该修订，不重复记录。
  - **节点变化摘要**：diff 之前先给一行节点变化的摘要，如 `Proxies: 26 changed (port), 1 renamed (A → B)`，`--json` 中为 `proxies` 字段。节点按名称对应；一减一增的节点除名称外完全相同时视为改名，不按位置等线索猜测，名称和其他字段同时变化的（如实测端口也随机的信息节点）照常列为一增一删；只列出变化的字段名，不显示字段的值（密码等）。原因是实测机场每次抓取都会给一部分节点换随机端口，信息节点的名称带着剩余流量，每天的 diff 都有几十行，只看 diff 难以判断“只是端口变了”还是“机场换了节点”。摘要只是帮助阅读，不过滤任何变化：端口变了内核就必须重新加载。
  - **长 diff 按段汇总**：diff 超过 300 行时（如换了机场、第一次加订阅）终端里不逐行显示，改为按顶层键列出增删的行数，提示用 `--full-diff` 查看全部（`config plan`、`config apply`、`config rollback`、`config edit`、`settings edit`、`tun on/off` 都支持）。几千行的 diff 没人会逐行看，分页器里翻也费劲，按段的汇总更能回答“改了哪些部分”。`--json` 中 `diff` 总是完整的，另有 `diff_sections` 汇总。
  - **长行截短**：订阅常把一个策略组写成一行（`{name: …, proxies: [全部节点]}`），这样的行出现在上下文里，或者只增删了其中几个节点时，整行照印会占满屏幕。终端显示时（不带 `--full-diff`）超过终端宽度的行要截短：一段连续的删除行与紧随其后的新增行按顺序配对，去掉两行相同的开头和结尾，只留行首（看得出是哪个策略组）和改动处前后各一小段，截取处尽量落在列表项之间；其余的长行（上下文、没有配对的改动行）只留开头。有行被截短时提示用 `--full-diff` 查看整行。`--json` 和发布的配置不受影响。不把订阅的单行写法改成每项一行：那样 diff 更准，但发布的配置就和机场给的写法不一样了。
- **emoji 原样输出**：yaml.v3 的编码器把基本平面以外的字符（emoji、国旗，UTF-8 中占 4 字节）当作不可打印字符，整个字符串改用双引号并写成 `\U0001F1ED` 这样的转义，配置和 diff 都难以阅读。编码前把这些字符换成文档中没有出现过的私用区字符，编码后再换回（`config/encode.go` 的 `keepAstral`），引号的写法与原来相同。
- **大订阅的内存**：tunlr 常与内核一起跑在小内存设备上，处理大订阅时的峰值内存按以下方式控制（4.4 MB、1 万节点的订阅，plan 同时存活的数据从约 245 MB 降到约 70 MB）：
  - 分段编码（`config/encode.go`）：yaml.v3 的编码器把处理过的事件留在队列里直到文档结束（上游 go-yaml 至今如此），一次编码整份配置时所有事件同时存活，约为配置大小的 45 倍。改为按顶层键、长列表再按约 2000 个节点一段，每段用新的编码器，拼接结果与整体编码逐字相同（有测试保证；遇到意料之外的写法时退回整体编码）。
  - 每份配置只解析一次（`config.Facts`）：需要重启的键、节点、geo 数据、面板地址从同一次解析中提取。新配置在合成时直接从节点树上提取，生效的配置解析一次；原先这些检查各自完整解析，每次约占配置大小十倍的内存。
  - 订阅没有别名和合并键时原地去掉锚点，不再整棵复制；订阅的统计在抓取时记入状态文件，列出订阅时不再解析内容。
  - 程序入口设置 Go 堆的软上限 64 MB（`GOMEMLIMIT` 可覆盖）：默认设置下进程占用约为实际所需的两倍，接近上限时运行时提早回收；超过上限不会出错，只是回收更频繁。
  - 剩下的大头是订阅的节点树本身（约为订阅大小的十倍），要再降需要让不改动的大列表绕过节点树，牵涉 mixin 的合并逻辑，暂不做。
- **热重载**（`pkg/core/mihomo.Client`、`Reloader`）：写入配置后经 API socket 调用 `PUT /configs?force=true`，`path` 填配置文件的绝对路径，然后用 `GET /version` 确认内核仍在响应。以下均为实测或读源码确认：
  - `path` 必须是绝对路径，且位于内核工作目录内，否则返回 400。
  - 必须带 `force=true`，否则端口等监听设置的变化不会生效；带上后端口变化即时生效。
  - 内核解析失败时返回 400 和错误信息，并继续运行原来的配置。
  - 内核应用配置的函数不返回错误，监听端口失败等问题不会反映在响应中，所以 204 只表示内核接受了这份配置。
  - 内核在重载时同步初始化 proxy-providers 和 rule-providers，新的 http provider 会现场抓取：地址不可达时实测重载耗时 3.4 秒。重载时限因此设为 2 分钟，超时按失败处理。
  - 内核退出后 socket 文件仍会留下，连接时被拒绝。socket 不存在和连接被拒绝都视为内核没有运行：配置照常发布，由内核启动时加载。
  - 重载失败（返回错误，或重载后不再响应）时，恢复原配置并让内核重新加载它，退出码 4。重载后内核不再响应时，即使随后连接被拒绝，也不能当作“没有运行”，同样回滚，让内核下次启动时用回原配置。
  - 热重载只调用 `executor.ApplyConfig`，不重建 API 服务；geo 定时器也只在启动时注册（读源码 `hub/hub.go`、`main.go`，并实测改 `secret`、`external-controller` 后重载不生效）。以下键变化时，apply 改为重启内核，plan 中说明“本次会重启内核”：`external-controller`、`external-controller-tls`、`external-controller-unix`、`external-controller-pipe`、`external-controller-cors`、`external-doh-server`、`secret`、`external-ui`、`external-ui-name`、`tls`、`geo-auto-update`、`geo-update-interval`。重启后做健康检查，失败时恢复原配置并再次重启，退出码 4。没有安装服务时照常写入，并提示需要重启内核。
- **回滚**：`config rollback` 不带修订号时，回到最近一个与生效配置不同的修订，所以连续执行两次会在两个版本间来回切换（与 `helm rollback` 相同）。`config rollback <号>` 恢复指定修订；历史由 `tunlr config` 列出（最新的在前，`*` 标出与生效配置相同的那一份）。回滚同样经过 `-t` 校验和确认，并记为新修订（来源为 rollback）。修订号递增、不复用。回滚恢复修订的内容后，独占的受管 mixin 按当前状态重新写入（如透明代理开关，见 8.3；面板的监听地址和 `secret`）。回滚只恢复配置文件，下一次 apply（包括订阅更新）会按当前的订阅和 mixin 重新合成。

## 7. 内核管理与救援

- **内核自升级的实际行为**（读 v1.19.33 源码 `component/updater/update_core.go`、`hub/route/upgrade.go`、`restart.go` 确认）：
  - 面板经 `POST /upgrade` 触发。内核从 `github.com/MetaCubeX/mihomo/releases/latest/download/` 下载，请求按规则分流，不经镜像，**不做任何校验**（只限制 32 MB、90 秒）。
  - 在二进制所在目录建 `meta-update/`、`meta-backup/`，备份后原地覆盖二进制（文件忙时先删除再创建），所以内核用户必须能写二进制和它所在的目录。
  - 资产名由编译时的 GOAMD64 决定（如 `mihomo-linux-amd64-v3-<版本>.gz`），升级后 CPU 级别不变。
  - 替换后以 `syscall.Exec` 在原进程内运行新二进制，PID 不变，systemd 看不到重启；新版本起不来时进程退出，才进入 systemd 的重启与救援。`POST /restart` 同样原地 exec。
- **按 CPU 指令集级别选择内核构建**：mihomo 的标准 amd64 构建要求 x86-64-v3（AVX2、BMI2、FMA 等），在更低级别的 CPU 上直接报错退出。虚拟机很常见这种情况：实测一台 KVM 虚拟机的 CPU 类型配置为通用型号时只暴露 x86-64-v1，标准构建无法运行。
  - 发布中有明确标注级别的构建 `mihomo-linux-amd64-v1|v2|v3-<版本>.gz`（另有不带级别的构建和旧名 `compatible`，不使用：带级别的资产名与面板升级时内核自己选的一致）。arm64 只有 `mihomo-linux-arm64-<版本>.gz`。
  - 读取 `/proc/cpuinfo` 的 flags 判断级别：v3 需要 avx avx2 bmi1 bmi2 f16c fma abm（lzcnt）movbe xsave；v2 需要 cx16 lahf_lm popcnt pni（sse3）sse4_1 sse4_2 ssse3；否则为 v1。
  - `doctor` 报告 CPU 级别；低于 v3 时提示虚拟机可改用 host 或更新的 CPU 型号以获得更好的性能（同时会带来 AES-NI）。
- **版本**：默认安装最新正式发布（不支持 alpha），`--version` 可以指定。
- **镜像下载，官方校验**：国内服务器直连 GitHub 下载很慢（实测 2 分钟只下完 22MB 中的 6.5MB），经 gh-proxy 等镜像只需约 12 秒。工具支持配置 GitHub 下载镜像（内核、geo 数据、面板，以及安装脚本），但**校验值必须取自官方来源**：从 `api.github.com` 读取 release 资产的 `digest`（sha256），镜像只负责传输字节，校验不一致则丢弃；GitHub 没有提供 digest 的资产拒绝下载。api.github.com 在国内通常可以直连。实现见 `pkg/core/download`。
- **运行身份与权限**：
  - 内核以系统用户 `tunlr` 运行（`useradd --system`），通过 systemd `AmbientCapabilities` 只授予 `CAP_NET_ADMIN` 和 `CAP_NET_BIND_SERVICE`。不用 `DynamicUser`：工作目录要由 root 写入配置又归内核用户所有，动态用户的 uid 不固定，目录还会被移到 `/var/lib/private`。
  - 为了让面板升级可用，`bin/` 归 `tunlr` 所有；工作目录（含 external-ui）同样归它所有。
  - **root 不执行内核用户可写的二进制**：`bin/mihomo` 可被内核改写，root 运行它就是提权。因此 `mihomo -t` 降权到 `tunlr` 用户运行（系统中没有这个用户，即尚未 `init` 时，以当前用户运行）。
- **已知可用版本**：`/var/lib/tunlr/kernels/<版本>/mihomo`，root 所有、0755，保留最近 3 个；状态文件记录每个版本的资产名、二进制的 sha256 和确认时间。工具安装的版本通过健康检查后加入，面板升级来的版本由 `_update` 核对后加入。
- **健康检查**：启动或重启后 15 秒内经 socket 得到 `/version` 响应且版本与预期一致，再等 3 秒，服务仍为 active 且 systemd 的重启计数（`NRestarts`）没有增加。
- **工具升级内核**（`tunlr core upgrade [--version <v>]`）：
  1. 下载并按官方 sha256 校验，解压到 `kernels/<版本>/`（此时尚未记为已知可用）。
  2. 降权用新内核对生效的配置执行 `-t`，不通过则停止（退出码 3），运行中的内核不受影响。
  3. 加锁，读入当前二进制备用（失败时换回）。
  4. 在 `bin/` 中写临时文件后 rename 替换，归 `tunlr` 所有。
  5. 服务在运行时重启并做健康检查；失败则换回原二进制、再次重启，退出码 4。服务没有运行时只替换，下次 `core start` 通过健康检查后才记为已知可用。
  6. 记为已知可用，清理多余的版本（`kernels/` 中不在记录里的目录，包括校验未通过、下载后没用上的）。
  - 只有 `kernels/` 中工具下载校验过、sha256 与 `bin/mihomo` 一致的版本，才会在健康检查通过后记为已知可用。
  - `tunlr core rollback` 换到上一个已知可用版本（不下载），同样经过 `-t`、重启和健康检查。
- **面板升级的善后**（`_update`）：`bin/mihomo` 的 sha256 不在已知可用记录中时，查询运行中的版本（`/version`），下载该版本对应的官方资产解压后比较：一致则记为已知可用；不一致则换成校验过的二进制（下载不到时，如 alpha 版本，换回最近的已知可用版本）并重启。内核没有运行时跳过，交给救援。
  - 实测（tunlr-dev）：面板经 `POST /upgrade` 升级时，内核经节点下载 github.com 失败（`io.Copy(): unexpected EOF`），原二进制不受影响；以 `tunlr` 身份换上官方 v1.19.33 后 `_update` 核对通过并记为已知可用；换上末尾多一个字节的官方二进制（仍能运行）后被换成校验过的版本。
- **救援**：
  - `tunlr-core.service` 配置 `Restart=on-failure`、`RestartSec=3`、`StartLimitIntervalSec=120`、`StartLimitBurst=5`，以及 `OnFailure=tunlr-rescue.service`（执行 `tunlr _rescue`）。达到重启上限后服务进入 failed，触发救援。
  - `_rescue`：透明代理的规则在内核退出时已由 `ExecStopPost` 清理（见 8.4），直连和 SSH 不受影响。当前二进制不是已知可用版本时，换回最近的已知可用版本，`reset-failed` 后启动并做健康检查。已经是已知可用版本仍然起不来时，多半是配置或环境问题：不循环重启，保持停止，在日志中说明原因。
  - `core upgrade` 进行中持有锁并自己处理回滚；`_rescue` 拿不到锁时直接退出，不与它争抢。
  - 代价：面板升级出问题时，会有一个从启动失败到自动回滚的短暂断网窗口；工具升级则没有。README 中说明这一取舍。
  - 实测（tunlr-dev）：把 `bin/mihomo` 换成立即退出的脚本后，systemd 重启 5 次（约 20 秒）进入 failed，触发 `tunlr-rescue.service`，换回已知可用版本并通过健康检查。
- **unit 的其他设置**：`ExecStart=/var/lib/tunlr/bin/mihomo -d /var/lib/tunlr/core`；`CapabilityBoundingSet` 与 `AmbientCapabilities` 相同；`NoNewPrivileges=yes`、`ProtectSystem=strict`、`ReadWritePaths=` 只含 `bin/` 与 `core/`、`ProtectHome=yes`、`PrivateTmp=yes`；`LimitNOFILE=1048576`。实测这些设置下 TUN 可以正常工作，不需要额外的设备或 capability（见 8.2）；MVP 3 增加 `ExecStartPre`、`ExecStopPost`（见 8.4）。实测 Debian 12（systemd 252）上 `AmbientCapabilities` 对非 root 用户生效。
- **定时任务的治理**：定时机制分三类，区别在于该不该由用户决定。
  | 类别 | 机制 | 可否关闭、改周期 |
  |---|---|---|
  | 护栏：保证不失联、不带病运行 | `tunlr-rescue.service`（启动失败后换回已知可用内核）、`tunlr-tun-guard`（开启 TUN 后的回滚定时器）、`tunlr-check.timer`（见 8.5）、面板升级的善后 | 不能。它们是“应用 → 确认，失败则回滚”的一部分 |
  | 资源更新：内容会过期 | 订阅（更新后发布）；geo 数据（内核按配置自己更新，`_update` 事后核对） | 能：订阅各自设周期，geo 数据在配置中设（mixin），都可以关闭 |
  | 不做定时 | tunlr 自身的升级 | 只能手动执行，不进 timer |
  - 资源更新只用一个 timer：`tunlr-update.timer` 每小时唤醒一次（`OnCalendar=hourly`、`RandomizedDelaySec=15min`），`_update` 读状态文件，对每个订阅判断“上次执行之后是否已过一个周期”，对 geo 数据判断“内核更新之后是否核对过”，到期才执行，执行后把时间和结果记下来。没有到期的更新时什么都不做，也不输出，journal 中只留真正执行过的更新。与每种资源一个 timer 相比：改周期只改设置、不用重写 unit 和 `daemon-reload`；新增资源只加一个设置项；下次执行的时间由工具自己算，`status` 和菜单直接显示；各更新顺序执行、不会同时运行；关机错过的更新在开机后的第一次唤醒时补上，不需要 `Persistent=`。代价是不能指定在某个时刻执行，服务器上的更新不在乎几点，可以接受。
  - 周期的值为小时或天数（`6h`、`1d`、`7d`，1 小时到 365 天）或 `off`。用户通过命令或菜单修改，不需要编辑文件。
    - **订阅**：周期是每个订阅自己的属性，存在状态文件中（`update`，默认 `1d`），添加时指定（`sub add --update`，交互时从列表选），之后用 `update set sub <name> <period>` 修改。未启用的订阅也按自己的周期更新（切换过去时内容是新的，机场的流量和到期信息也是新的），只有启用的订阅更新成功后才发布配置。下次到期 = 上次抓取（成功或失败，取较晚者；从未抓取过时为添加时间）+ 周期；上次失败时最多 6 小时后再试，不必等满一个周期。
    - **geo 数据**：周期是配置的一部分（`geo-auto-update`、`geo-update-interval`，单位小时），模板中为 `true`、`168`（上游每周更新一次），订阅或 mixin 写了的以它们为准，`tunlr update` 中显示生效的值（`168` 显示为 `7d`）。见下面的“geo 数据更新”。
    - `tunlr-update.timer` 安装后总是启用：即使没有订阅定时更新、配置中也关闭了 geo 数据的自动更新，面板仍可随时让内核更新 geo 数据或升级内核，之后都要核对；没有要做的事时它什么都不做。
  - `tunlr update run [sub|geo]` 不看周期立即执行：`sub` 抓取全部订阅（包括设为 `off` 的），`geo` 先调用 `POST /configs/geo` 让内核立即更新（内核没有运行时只核对已有的文件），再核对；输出与定时执行相同。`tunlr update` 列出每个订阅和 geo 数据的周期、上次执行的时间和结果、下次到期的时间；geo 数据的“上次执行”是上次核对，“下次到期”是内核下一次更新的大致时间（最新的 geo 文件的修改时间 + 周期，由内核决定），表格下另有一行说明最新的 geo 文件的时间。
  - 执行顺序：先逐个抓取到期的订阅（失败时保留上一次的内容，原因记在订阅上），启用的订阅抓取成功后按它和 mixin 合成并发布（同 `config apply --yes`；有变化才发布，检查不通过则保持当前配置）；再核对 geo 数据（结果记入状态文件）。执行了任何任务时最后做面板升级的善后（见上）：它只核对校验值，不单独设周期。
  - 任何一步失败都记录在日志中，继续后面的步骤，最后以非零退出码结束，让 systemd 记为失败。每一步的结果（ok、skipped、failed 和说明）输出到 journal。
- **geo 数据更新**（读源码 `component/updater/update_geo.go` 并实测）：
  - 内核的 geo 数据只能由内核自己的更新器重新加载（`mmdb.ReloadIP` 等只在更新器中调用）；工具替换文件后要重启内核才生效。
  - `POST /configs/geo` 同步执行：内容与磁盘上的相同时直接返回 204，不写文件也不记日志（之前“没有结果”的原因）；单个文件的下载时限 90 秒，经 gh-proxy 实测一次 64 秒成功、一次 90 秒超时。
  - 内核自己的定时更新（`geo-auto-update`）只在启动时注册；启动时 GeoIP 文件的修改时间早于一个间隔就立即更新。它下载后不做任何校验。
  - 设计（2026-10-29 讨论后决定）：geo 数据的来源和更新频率是配置的事，订阅可以指定自己的来源（规则依赖它），所以都交给配置和内核，工具不再用自己的设置覆盖它们。原来的 `geo-update`/`geo-updater` 设置、`update set geo --by`、独占的 `tunlr:geo` 全部删除，不做兼容。工具只做内核做不到的事——核对：
    - `_update` 每小时比较最新的 geo 文件的修改时间与上次核对的时间，文件更新过才核对；上次核对失败的，6 小时后再试。
    - 核对按生效的配置中的 `geox-url`：地址（去掉镜像前缀后）是 GitHub release 资产（`releases/download/<tag>/<名称>` 或 `releases/latest/download/<名称>`）的，从 GitHub API 取官方 sha256 比对工作目录中内核正在使用的文件；不一致时下载校验过的文件（经镜像校验不通过时改为直连）替换并重启内核（内核没有运行时不启动它）。其他来源无从校验，结果中注明 “from a custom source, not verified”。没有写 `geox-url` 的文件按内核的默认地址（meta-rules-dat 的 `latest`）。
- **状态以实际为准**：版本、TUN 状态等一律通过 API 查询内核实际情况；工具自己的记录只作为“已知可用的历史”。
- 注意：内核 API 并不稳定（例如曾有标注为 break 的 `/proxies` 行为变更），API 调用集中封装在 `pkg/core/mihomo` 的 client 中，并随 mihomo 发版做兼容性测试。

## 8. 透明代理

> 8.3～8.6 是 MVP 3 的设计，根据 8.2 的实测结果写成，已经实现，实现后的实测见 8.7。MVP 3 只做本机模式，网关模式是 MVP 5，实测见 8.9，设计见 8.10。

### 8.1 方案：TUN + auto-redirect

- 配置：开启 `auto-route` 与 `auto-redirect`，**关闭 `strict-route`**（它会让本机无法被其他设备访问）。**不用 `route-exclude-address` 排除私有地址段**：直连网段本来就不进 TUN，整段排除反而让局域网中的 DNS 服务器绕过劫持（见 8.2）。
- 理由：auto-redirect 用 nftables 处理 TCP，避开了 TUN 回包被 rp_filter 丢弃的问题；sing-box 文档称其路由更好、性能优于 tproxy。内核自己的规则覆盖了大部分场景，工具只补实测中缺的几条（见 8.4）。
- 已知局限：auto-redirect 只接管 TCP，UDP 仍走 TUN 路由。绑定 0.0.0.0、又不指定回包源地址的入站 UDP 服务会受影响（见 8.2）。
- 结论：维持 TUN 方案，不实现 tproxy（第 14 节的待决事项由此关闭）。

### 8.2 实测

1. 从外部 SSH 到服务器
2. 访问服务器上的 TCP 服务
3. 访问服务器上的 UDP 服务
4. 服务器上 Docker 容器的联网

全部通过则维持默认方案；只有入站 UDP 等场景确实不满足时，才实现自写 tproxy（`tun on --mode tproxy`）。

**实测结果**（tunlr-dev：Debian 12，Linux 6.1，mihomo v1.19.33，Docker 默认网桥；`stack: mixed`、`auto-route`、`auto-redirect`、`strict-route: false`、`dns-hijack: [any:53, tcp://any:53]`，经 mixin 开启）：

- 热重载即可开关 TUN，无需重启。内核以 `tunlr` 用户运行、只有 `CAP_NET_ADMIN`，就能建立 TUN 网卡 `Meta`、ip rule（优先级 9000～9010，路由表 2022）和 nft 表 `inet mihomo`。关闭 TUN 或正常停止内核时全部清理干净。
- 直连网段（本机网段、Docker 网桥）不进 TUN：ip rule `lookup main suppress_prefixlength 0` 先查主路由表中默认路由以外的路由。所以不需要用 `route-exclude-address` 排除它们。
- **用 `route-exclude-address` 排除整个私有地址段有副作用**：DNS 服务器在局域网时（测试机为 192.168.0.11），DNS 请求不经 TUN，`dns-hijack` 不生效，宿主和容器拿到被污染的地址，域名规则全部失效。`inet mihomo` 中排除地址的 `return` 排在 DNS 的 `dnat` 之前。
- 不排除任何地址（模拟公网 VPS：SSH 客户端经网关进来，回包会被路由进 TUN）：

  | 项目 | 结果 |
  |---|---|
  | 1. 外部 SSH | 通过 |
  | 2. 宿主上的 TCP 服务 | 通过 |
  | 3. 宿主上的 UDP 服务 | 绑定具体地址的通过；绑定 0.0.0.0 且不指定回包源地址的失败：回包的源地址按 TUN 路由选成 198.18.0.1，与 conntrack 记录对不上，进入 TUN。加 `ip rule sport <端口> lookup main` 后通过。WireGuard、quic-go 等会指定回包源地址的服务应不受影响（未实测） |
  | 4. Docker 容器出站 | 通过：fake-ip、域名规则命中 |
  | 4'. Docker 发布的端口（入站） | 失败：容器的回包是转发流量，进入 TUN。用 conntrack 给“从 TUN 以外进入、目标为本机”（`fib daddr type local`，此时尚未 DNAT）的新连接打标记，回包带标记、经 ip rule 查主路由表后通过 |

- 内核被 `SIGKILL`：TUN 网卡随之消失，残留的 ip rule 指向空路由表，无害；残留的 `inet mihomo` 表会把容器的 TCP 和 DNS 重定向到已不存在的监听端口，中断到 systemd 拉起内核为止（拉起后规则重建，不重复）。内核起不来时，救援需要清理这张表。

### 8.3 开关与配置

- **开关**：`tunlr tun on`、`tunlr tun off`；不带子命令的 `tunlr tun` 显示状态。`--mode` 预留给将来的 tproxy，现在不提供。
- **开关存在状态文件中**（`state.json` 的 `tun`），不放在 `settings.yaml`：手改设置文件打开 TUN 会绕过防失联检测。状态有三种：`off`、`pending`（已发布、检测中，带回滚期限）、`on`。新增的字段可以省略，旧状态文件视为 `off`，不需要迁移。
- **配置复用分层合成**，`tun on/off` 本身只改开关，然后走与 `config apply` 相同的流水线（`-t` 校验、原子写入、热重载、失败回滚、记入历史）。实测热重载即可开关 TUN，`tun` 不加入需要重启的键。
  - 模板（用户可以用 mixin 覆盖）：`stack: mixed`、`auto-detect-interface: true`、`dns-hijack: [any:53, tcp://any:53]`，附注释说明为什么不排除私有地址段。
  - 由 `tunlr:tun` 独占（mixin 写了就拒绝发布，提示用 `tunlr tun on/off`）：`tun.enable`（取自开关：`pending` 与 `on` 为 true）、`auto-route: true`、`auto-redirect: true`、`strict-route: false`、`device: tunlr0`、`iproute2-table-index: 2022`、`iproute2-rule-index: 9000`。工具补的规则、清理和检测都依赖这些值；固定网卡名也让 `doctor` 和日志一眼认出它。
  - 不写进 `mixin.d`，理由见第 6 节“受管 mixin”。
- **回滚与开关**：`config rollback` 恢复旧修订的内容后，再按当前开关写入独占的受管 mixin（与合成时最后一层相同）。否则回滚到开关不同的修订，配置就与工具的规则和防失联状态对不上。`config plan` 照常显示差异。
- **面板运行时切换**（见 3.3）：面板打开 TUN 不写回配置，也不经防失联检测。工具的规则不依赖开关（见 8.4），所以面板打开时同样生效。`status` 在内核实际状态与开关不一致时提示，执行 `tun on/off` 即以开关为准。

### 8.4 工具补充的规则及其生命周期

- 规则放在独立的 `inet tunlr` 表中，模板 `templates/nft/tunlr.nft`，经 `nft -f` 原子加载，整表删除即清理：
  - **入站连接的回包走主路由表**：从 `tunlr0` 以外进入、目标为本机（`fib daddr type local`，此时尚未 DNAT）的连接，在原方向（`ct direction original`）的包经过时打上 conntrack 标记。不限于新连接：加载本表之前已经建立的连接（如执行 `tun on` 的 SSH 会话）在对方发来下一个包时也会标记，所以 `tun on` 在发布前先加载一次本表；属于这些连接的回包（本机进程发出的走 `output` route 链，容器发出的走 `prerouting`）带上包标记，`ip rule fwmark … lookup main`（优先级 8999，早于内核的 9000）。这修好了 Docker 发布的端口，也让 SSH 等所有入站连接不依赖内核规则的细节。
  - 标记只用一个高位 `0x10000000`，按位设置（`meta mark set meta mark | 0x10000000`），不覆盖其他程序的标记：Tailscale 用 `0xff0000` 段，kube-proxy 用 `0x4000`、`0x8000`，内核的 auto-redirect 用 `0x2023`、`0x2024`。
  - ip rule 由 iproute2 的 `ip` 命令维护（IPv4、IPv6 各一条）。
- **与内核服务同生共死**，不依赖开关：规则在 TUN 关闭时无害（标记的回包本来就走主路由表），这样面板临时打开 TUN 也有效。
  - `tunlr-core.service` 增加 `ExecStartPre=-+/usr/local/bin/tunlr _prestart`：加载 `inet tunlr` 表和 ip rule（`+` 表示以 root 运行，不受 `User=` 和沙箱限制；`-` 表示失败也照常启动内核，不让规则的问题拖垮代理本身）。
  - 增加 `ExecStopPost=-+/usr/local/bin/tunlr _poststop`：删除 `inet tunlr` 表和工具的 ip rule；内核的 `inet mihomo` 表和优先级 9000～9010 的 ip rule 如果还在（内核被 `SIGKILL` 或崩溃），一并删除。`ExecStopPost` 在进程以任何方式退出后都会执行，所以实测中 `SIGKILL` 后容器断网的问题由此消除，`_rescue` 不必再单独清理。
  - 这两个命令不取文件锁（`tunlr core start` 持锁调用 `systemctl start` 时会死锁），操作本身幂等。
- **规则被他人清空**：例如重新加载 Debian 的 `nftables.service`，其配置以 `flush ruleset` 开头，会连同内核的表一起清空。增加 `tunlr-check.timer`（每 5 分钟），执行 `_check`：内核在运行而 `inet tunlr` 不见了，重新加载；内核开着 TUN 而 `inet mihomo` 不见了，重启内核由它重建。另外兜底处理回滚定时器没有生效的情况：开关为 `pending` 且过期一分钟以上时关闭 TUN。这些都是透明代理的事，所以这个 timer 随开关启停：`tun on` 布置回滚定时器后启用，`tun off` 停用；开关被回滚定时器或 `_prestart` 关闭时，`_check` 下次执行时发现开关已关，停用自己（不在 `ExecStartPre` 中调用 systemctl）。面板临时打开的 TUN 不在保护范围内。

### 8.5 开启流程与防失联

`tun on`：

1. 前置检查，不通过则退出码 3：服务已安装且内核在运行（不运行就无法检测，提示先 `core start`）；`/dev/net/tun`、`nft`、`ip` 可用；优先级 8999～9010 没有他人的 ip rule；启用了 UFW、firewalld 时给出警告（未实测，见 8.8）。
2. 显示 `config plan`（diff 中是 `tun.enable` 的变化）并确认；非交互模式需要 `--yes`，`--dry-run` 只预览。
3. 加锁，开关记为 `pending`（期限为现在加 3 分钟），用 `systemd-run --on-active=3min --unit=tunlr-tun-guard` 布置回滚定时器，执行 `tunlr _tun-guard`。
4. 加载工具的规则（见 8.4，让已经建立的连接也能标记），发布配置，内核热重载打开 TUN。发布失败时流水线自己回滚，开关恢复原状，退出码 4。然后释放锁，检测不需要持锁，这样卡住的检测不会挡住回滚定时器。
5. 检测（30 秒内）：
   - 内核实际已打开 TUN（经 API 查询），`tunlr0`、`inet tunlr`、`inet mihomo` 都在；
   - 经系统的 DNS 设置能解析测试域名；
   - 测试地址至少一个能访问，默认一个国内（`https://www.baidu.com`）、一个国外（`https://www.gstatic.com/generate_204`），逐个报告结果；只有全部失败才算失败，国外失败可能只是节点问题；
   - **SSH 会话仍然通畅**：在 SSH 会话中运行时（有 `SSH_CONNECTION`），用 `ss -Htni state established src <本机> dst <客户端>` 记下这条连接的 `bytes_acked`，向终端（标准错误）输出一行文字，8 秒内 `bytes_acked` 增加即通过（内核不报告它时退回看 Send-Q 归零）。标准错误不是终端、在 tmux 或 screen 中（输出不一定经过这条连接）、找不到这条连接时跳过。SSH 一旦断开，进程收到 SIGHUP 退出，留给回滚定时器处理。
6. 全部通过：加锁，开关记为 `on`，停止回滚定时器。任何一项失败：立即关闭（开关 `off`、发布、停止定时器），报告失败的项，退出码 4。

- `_tun-guard`：开关仍为 `pending` 且已过期限时，记为 `off` 并发布，记日志；已经是 `on` 或 `off` 时什么都不做。
- **重启机器的情况**：SSH 断开后，用户很可能从云平台控制台重启服务器，而 `systemd-run` 的临时定时器不会保留。所以 `_prestart` 也检查：开关为 `pending` 且已过期限时，先把配置改为关闭 TUN，再让内核启动。这里只尝试加锁、不等待（锁被占用说明有人正在操作）。
- `tun off`：加锁，开关记为 `off`，发布，停止可能存在的回滚定时器。不做检测：关闭就是恢复。
- 用户不需要手动确认。8.2 的实测中 SSH 入站本来就没有受影响，加上 8.4 的回包规则，SSH 失联主要是兜底场景。

### 8.6 与其他功能的关系

- `config apply`、`_update`、`sub use` 照常合成，`tunlr:tun` 随开关写入，定时更新不会改变 TUN 状态。
- `status`：开关（`pending` 时显示剩余时间）、内核实际的 TUN 状态、两张表是否存在。
- `uninstall`：停止服务时 `_poststop` 清理规则（服务没在运行或 unit 是旧版时再直接清理一次）；删除 `tunlr-check.timer`，停止可能存在的回滚定时器；开关记为 `off`，重新 `init` 后 TUN 保持关闭。
- `doctor`（MVP 4）复用第 1 步的前置检查。
- 代码：规则模板、加载与清理、前置检查在 `pkg/core/netfilter`；开关与开启流程在 `pkg/core/setup`；CLI 只有 `internal/cli/tun.go`。nft 模板做 golden 测试，规则本身按 CLAUDE.md 在 network namespace 中做集成测试。

### 8.7 实现后的实测

tunlr-dev（环境同 8.2；SSH 客户端 192.168.11.41 不在服务器的直连网段，回包按默认路由本会进 TUN）：

| 项目 | 结果 |
|---|---|
| nft 规则（network namespace 集成测试） | 通过：重复加载幂等；模拟内核残留后 `Down` 清理干净，他人的 ip rule 保留 |
| `init` 升级旧安装 | unit 带上 `_prestart`、`_poststop`，`tunlr-check.timer` 随 TUN 开关启用，规则随内核启动加载 |
| `tun on`（SSH 中执行） | 通过：TUN、DNS、HTTP、SSH 四项检测通过，开关为 `on`，回滚定时器撤掉 |
| 开启前已经建立的 SSH 连接 | 先删掉工具的表和规则，在同一会话中 `tun on`：SSH 检测通过 |
| SSH 检测的失败路径 | 用 nft 丢弃 sshd 发往客户端的包后 `tun on`：SSH 检测失败，TUN 立即关闭，退出码 4 |
| 容器出站、Docker 发布的端口（入站） | 通过 |
| 内核被 `SIGKILL` | 1 秒内 `_poststop` 删除残留的 `inet mihomo` 表和 ip rule；systemd 拉起后重建，规则没有重复 |
| 两张表都被删除后 `_check` | 重新加载 `inet tunlr`，重启内核重建 `inet mihomo` |
| `tun on` 在检测阶段被 `SIGKILL` | 开关停在 `pending`，回滚定时器到期后关闭 TUN |
| 重启机器（回滚定时器丢失，`pending` 已过期） | 撤掉定时器、把期限改到过去后重启内核服务：`_prestart` 先关闭 TUN，内核以关闭状态启动 |

### 8.8 尚未实测

- 真实的 UDP 服务（WireGuard、hysteria2 服务端）：按原理不受影响。若受影响，再加按端口放行的设置（`ip rule sport <端口> lookup main`）。
- IPv6：测试机没有公网 IPv6。日志显示 TUN 没有 IPv6 地址（`Meta([198.18.0.1/30],[])`），有公网 IPv6 的服务器上 IPv6 流量可能不经 TUN。
- UFW、firewalld 启用时：auto-redirect 把容器的 TCP 重定向到本机端口，可能被入站策略拦下。
- Ubuntu 的 systemd-resolved（`127.0.0.53`）：DNS 先到本机的 resolved，再由它向上游查询；内核的 ip rule 对目的端口 53 有专门处理，应当仍能劫持。
- 以本机方式安装的 Tailscale：它的 ip rule 优先级为 52xx，排在 TUN 之前，应当互不影响。

### 8.9 网关模式实测（MVP 5 设计前，设计见 8.10）

tunlr-dev（环境同 8.2，装有 Docker：`ip_forward` 由 Docker 在运行时设为 1，`ip filter FORWARD` 策略为 drop）。用 network namespace 加 veth 模拟一台局域网设备（10.77.0.2，网关 10.77.0.1，DNS 设为 223.5.5.5）。模拟网段不做 NAT，所以 TUN 关闭时出站本来就不通。

内核的 `inet mihomo` 表决定了转发流量怎么走：

- 来自本机直连网段（`inet4_local_address_set`，含局域网网段）的 53 端口请求，`dnat` 到 TUN 中的 DNS 地址 `198.18.0.2`，因此成为转发流量。
- 目标为直连网段的流量 `return`，不进 TUN。
- 其余 TCP 在 `prerouting` 被 `redirect` 到内核的本机端口，成为本机入站，不经过转发链。
- UDP、ICMP 按内核的 ip rule 路由进 `tunlr0`，经过转发链。

| 项目 | TUN 关闭 | TUN 开启 | TUN 开启 + 放行转发 |
|---|---|---|---|
| DNS（发往 223.5.5.5） | 失败 | 失败（`dnat` 后被转发链丢弃） | fake-ip，被劫持 |
| DNS（发往网关自身） | 失败 | 失败 | fake-ip，被劫持 |
| TCP（用 IP 访问） | 失败 | 通过（被 redirect） | 通过 |
| TCP（用域名访问） | 失败 | 失败（DNS 不通） | 国内、国外都通过 |
| UDP（NTP，国内、国外） | 失败 | 失败 | 通过 |
| ping | 失败 | 失败 | 通过 |
| 访问网关自身的 SSH | 通过 | 通过 | 通过 |

“放行转发”是按 Docker 文档的做法，在 `DOCKER-USER` 链中加入两条规则：`-i <局域网网卡> -o tunlr0 -j ACCEPT` 及反方向。在自己的 nft 表中 accept 无效：同一个 hook 上任何一张表 drop 就会丢包。

结论：开启 TUN 后，网关模式缺的只有两样：`ip_forward`，以及转发链策略为 drop 时的放行。auto-redirect 和 `dns-hijack` 已经覆盖了 TCP 和 DNS，不需要 NAT，也不需要改 `dns.listen`。

尚未实测：没有 Docker（转发链策略为 accept）的机器、局域网设备与网关在同一网段（旁路由的实际情形，实测用的是单独的 veth 网段）、rp_filter 为严格模式、IPv6。

### 8.10 网关模式（MVP 5）

局域网设备把这台服务器设为默认网关（以及 DNS），它们的流量经 mihomo 分流，即常说的旁路由。按 8.9 的实测，开启 TUN 后 auto-redirect 和 `dns-hijack` 已经处理了转发来的 TCP 和 DNS，网关模式只需要补：IPv4 转发、转发链策略为 drop 时的放行，以及内核不在时不让局域网流量直接出去。不需要 NAT，不改 mihomo 的配置。

- **命令**：`tunlr tun gateway` 显示状态；`tunlr tun gateway on [--interface <网卡>]`；`tunlr tun gateway off`。菜单放在“Transparent proxy”分类中。`init` 的用途问题选“当网关和代理”时，在 init 成功、内核已经运行之后走同一个流程；没有订阅时不问（内核不会启动，TUN 无法检测），见第 10 节“家庭网络中的用法”。
- **开关**存在 `state.json` 的 `gateway`（局域网网卡、开启前 `ip_forward` 的值），理由同 TUN 开关（8.3）。字段可以省略，旧状态文件视为关闭，不需要迁移。
- **局域网网卡**：默认取 IPv4 默认路由所在的网卡（旁路由的情形，局域网与上行是同一块网卡），可用 `--interface` 指定。不能是 `lo`、`tunlr0`。交互模式下不给 `--interface`、网关模式还没开启、且候选网卡多于一块时（候选同 `init` 的用途问题：已启用、有 IPv4 地址，排除回环、TUN 和常见的虚拟网卡），在列表中选择，默认路由所在的排在最前，有公网地址的标出；`--yes` 时不问，取默认路由所在的网卡。菜单的“Turn gateway mode on”走同一个流程。
- **只做 IPv4**：测试机没有 IPv6（见 8.8），`ip6` 转发不开启也不放行。

**`tun gateway on`**：

1. 前置检查，不通过则退出码 3：TUN 的前置检查（8.5 第 1 步）；局域网网卡存在且已启用。该网卡的 `rp_filter` 为严格模式（1）时警告（未实测）。
2. TUN 没开时，先走 `tun on` 的完整流程（plan、确认、防失联检测），通过后再继续；TUN 开启失败则网关也不开启。
3. 显示将要做的改动并确认；非交互模式需要 `--yes`，`--dry-run` 只预览。
4. 加锁，依次：
   - 记下 `net.ipv4.ip_forward` 的当前值，写入 `/etc/sysctl.d/90-tunlr-gateway.conf`（`net.ipv4.ip_forward = 1`，重启后仍生效）并立即生效。
   - 加载 `inet tunlr_gateway` 表（`templates/nft/gateway.nft`，`nft -f` 原子替换）：从局域网网卡进来、不出 `tunlr0`、目标不是私有地址段的转发流量丢弃。TUN 正常时局域网流量都进 `tunlr0`，这条规则不起作用；内核停止、崩溃或面板临时关闭 TUN 时，局域网设备断网，而不是悄悄直连（失败时关闭）。私有地址段（`10/8`、`172.16/12`、`192.168/16`、`100.64/10`、`169.254/16`）照常转发：内核本来就不代理直连网段，Docker 发布的端口在转发前已 DNAT 到容器地址。
   - 存在 Docker 的 `DOCKER-USER` 链时，在其中插入两条放行规则（`-i <网卡> -o tunlr0` 及反方向，带注释 `tunlr`）。同一个 hook 上任何一张表 drop 都会丢包，自己的表中 accept 无效，只能按 Docker 文档放在这条链里。**这是项目中唯一调用 `iptables` 的地方**（Docker 的链由 iptables 管理）；没有这条链时不调用。
   - 开关记为开启。任何一步失败，撤销已做的步骤，退出码 4。
5. 输出局域网设备应设置的网关和 DNS 地址（局域网网卡的 IPv4 地址）。

不做防失联检测：网关不改变本机自己的流量，SSH 不受影响。从服务器上也无法检测局域网设备是否能用，`doctor` 检查各项是否到位。

**`tun gateway off`**：加锁，删除 `DOCKER-USER` 中的两条规则、`inet tunlr_gateway` 表和 sysctl 文件；`ip_forward` 开启前为 0、且没有 `DOCKER-USER` 链时恢复为 0（Docker 需要转发，有它时保持 1）；开关记为关闭。TUN 保持开启。

**生命周期**：

- 网关的规则不随内核停止删除（`_poststop` 不动它们），这样内核不在时仍是失败时关闭。
- `_prestart`（含开机）在开关开启时重新加载 `inet tunlr_gateway` 表和 `DOCKER-USER` 规则（nft 规则不跨重启保存）。Docker 晚于内核服务启动时，`DOCKER-USER` 链那时还不存在，由 `_check` 补上。
- `_check`（随 TUN 启用，每 5 分钟）在开关开启时检查表和 `DOCKER-USER` 规则，缺了就补（例如 `flush ruleset`、Docker 重建了链）。
- `tun off` 先关闭网关（同一把锁中），并在确认时说明；防失联回滚关闭 TUN 时同样先关闭网关。
- `uninstall` 关闭网关，删除 sysctl 文件。

**其他**：

- `status` 显示网关开关和网卡；`tunlr tun`（连同透明代理）和 `tunlr tun gateway` 显示各项是否到位（转发、sysctl 文件、表、`DOCKER-USER` 规则）。
- `doctor` 的 install 组在网关开启时检查：`ip_forward` 为 1、sysctl 文件、`inet tunlr_gateway` 表、有 `DOCKER-USER` 链时的放行规则、局域网网卡存在、`rp_filter` 不是严格模式、TUN 开启。
- 代码：nft 模板与 `DOCKER-USER` 规则在 `pkg/core/netfilter`；开关、sysctl 与流程在 `pkg/core/setup`；CLI 在 `internal/cli/gateway.go`。
- `--json` 时 TUN 必须已经开启（退出码 3，提示先 `tunlr tun on`）：两段流程各有一份 JSON 输出，无法合并。
- 再次执行 `tun gateway on` 会补上缺少的部分，不给 `--interface` 时沿用正在使用的网卡（而不是换到默认路由所在的网卡）；换 `--interface` 时先撤掉旧网卡的规则，开启前的 `ip_forward` 仍按最初记下的值。

**实现后的实测**（2026-10-28，tunlr-dev，Debian 12 + Docker，局域网设备仍用 netns + veth 模拟）：

| 项目 | 结果 |
|---|---|
| TUN 关闭时 `tun gateway on --yes` | 先走完 `tun on`（含检测），再开启网关，一次完成 |
| 局域网设备：DNS（国内、国外） | fake-ip，被劫持 |
| 局域网设备：HTTPS 国内、国外，UDP NTP，ping | 都通过 |
| 内核停止（`tunlr core stop`） | 局域网设备的 TCP、ping、DNS 都不通（失败时关闭），服务器自己照常联网；`core start` 后恢复 |
| 删掉网关表和一条 `DOCKER-USER` 规则 | `doctor` 报 fail；`_check` 补上，再次执行无事可做 |
| `tun gateway off` | 表、`DOCKER-USER` 规则、sysctl 文件都删除；有 Docker，`ip_forward` 保持 1；TUN 保持开启 |
| 再次 `tun gateway on` 两次 | 第二次报告已经开启，不做改动 |
| `tun off` | 先关闭网关，确认前说明；表与规则都删除，`tunlr-check.timer` 停用 |
| `TestNetns`（真实 nft、netns 中的 iptables） | 通过 |

仍未实测：同 8.9 末尾所列（没有 Docker 的机器、同网段的旁路由、rp_filter 严格模式、IPv6），以及重启后由 `_prestart` 重新加载。

### 8.11 若将来实现 tproxy，需要处理的 Docker 冲突

- 容器发布端口的回包会被劫持 → `ct direction reply return`
- tproxy 挂在 mangle（-150），早于 Docker 的 DNAT（-100） → `fib daddr type local return`
- 显式用 `iifname` 列出要拦截的接口（docker0、`br-*` 是否代理属于策略选择）
- 用 mihomo 的 `routing-mark` 防止回环；局域网与保留地址直接 return
- 规则放在独立的 `inet tunlr` 表中，清理时整表删除；`_check` 检测规则是否被他人 flush

### 8.12 局域网设备：列表与分流

家庭网络中（第 10 节“家庭网络中的用法”），有的设备不该走代理节点：游戏主机、IoT 设备、家人的手机。用户要能看到哪些设备在用这台服务器，并逐台决定它们走代理还是直连。

**前提：把这台服务器设为代理或网关的设备，流量一律经过 mihomo**。直连也由 mihomo 按规则直连，不在系统层绕过内核。不需要经过 mihomo 的设备，网关直接用路由器即可（手动设置，或在路由器上给单台设备设 DHCP 选项）。

**设备的两种走法**：

| 走法 | 含义 | 实现 |
|---|---|---|
| `proxy`（默认） | 按规则分流 | — |
| `direct` | 这台设备的流量全部直连 | 受管 mixin 在规则最前插入按来源匹配、`DIRECT` 的规则 |

- 按来源 IP（`SRC-IP-CIDR,<ip>/32,DIRECT`）匹配。内核没有按来源 MAC 的规则（v1.19.33 实测 `SRC-MAC` 报 unsupported rule type），所以设为直连后提示在路由器上为它保留地址。记录同时保存 MAC：设备换了地址时列表按 MAC 认出它，提示规则仍按旧地址匹配、再执行一次 `filter device` 即更新。
- 网关模式与代理端口都适用：两种方式下内核看到的来源都是设备的地址。
- 规则由提供默认值的受管 mixin `tunlr:filter` 插入（与其他过滤项一起，见 8.13），用户的 mixin 可以覆盖或删除；`config plan`、`tunlr config` 列出它的内容。名单变化经流水线发布（diff、确认、`mihomo -t`），失败时回滚，名单恢复原状。

**不做系统层绕过**（2026-10-28 讨论后决定）：曾考虑让名单中的设备完全不经过内核（内核停止时照常上网），在 tunlr-dev 上验证过可行：内核的 `tun.exclude-mac-address` 只跳过 TCP 重定向和 DNS 劫持、不改路由，还要自己按 MAC 打标记加 `ip rule`、把 DNS 转给路由器、放行转发并做 masquerade。不做的理由：依赖 sing-tun 的 nft 表、ip rule 位置和标记位等内部行为，内核升级后可能悄悄失效且影响整个局域网；需要它的场景（路由器不能为单台设备设网关、设备自己也改不了）不多，多数情况下让设备直接用路由器做网关即可。

**设备列表**：

- 来源：
  - **nft 动态集合**（主要）：表 `inet tunlr_devices`（`templates/nft/devices.nft`），prerouting 的 raw 优先级（早于内核的 TCP 重定向，看到原始目标地址）把来源的 `MAC . IPv4` 用 `update` 记入两个集合：`gateway`（从集合 `lan` 中的网卡进来、目标不是本机）与 `proxy`（私有地址来的、目标是本机且端口在集合 `ports` 中）。每个包都刷新计时，24 小时没有流量即过期，由剩余时间算出最后出现的时间。只记录，不改动包。
  - mihomo 的 `/connections`：当前连接数。
  - 邻居表（`ip -j neigh`）：给记录中没有 MAC 的设备补全；`--all` 时列出其他局域网设备（排除 TUN、Docker、veth 等虚拟网卡上的邻居），方便提前设置。
  - 名称：用户起的名称优先，其次向路由器（IPv4 默认路由的下一跳）反向解析，取第一段；连接打上工具的标记，透明代理开启时不被内核劫持 DNS。总时限 0.5 秒：tunlr-dev 的上级路由器不提供 DNS，每次都等到超时；家用路由器上的解析尚未实测。
- 状态：`online`（5 分钟内有流量）、`idle`（24 小时内来过）、`away`（有记录但 24 小时内没来过）；`--all` 中没有使用本机的为 `not using`。服务器重启后集合清空，设备再次使用即重新出现。
- `inet tunlr_devices` 在网关或代理端口局域网访问开启时加载，两者都关闭时删除。集合 `lan`、`ports` 随开关更新（`tun gateway on/off`、`proxy lan on/off`），只替换这两个集合，不重建表，已记下的设备保留；`ports` 取生效配置的 `mixed-port`，mixin 改了端口时由 `_prestart` 和 `tunlr devices` 更新。`_check` 补缺，`uninstall` 删除；`_poststop` 保留它。记录设备是附带功能，表加载失败只记日志，不影响开关。
- 只在本机显示，不保存历史；持久保存的只有用户设置过走法或名称的设备（状态文件的 `devices`：MAC、IP、名称、走法）。没有名称、走法为默认的记录自动去掉。
- 在 tunlr-dev 上实测（netns 模拟的设备经代理端口访问）：几秒内出现在列表中，状态 `online`；设为直连后内核对它的连接显示规则 `SrcIPCIDR`、链路 `DIRECT`；设回 `proxy`、`forget`、关闭代理端口的局域网访问后，规则、记录和表都已清理。
- 在 tunlr-dev 上实测网关一路（开启透明代理和网关，局域网网卡为 netns 的 veth）：设备上网后出现在列表中，用途 `gateway`、状态 `online`；设为直连后它的连接显示规则 `SrcIPCIDR`、链路 `DIRECT`，设回 `proxy` 后新连接重新按订阅的规则匹配；重启内核后已记下的设备仍在；`forget`、关闭网关和透明代理后表、规则、记录都已清理。

**命令**：

```
tunlr devices [--all]                    列出使用本机的设备（名称、IP、MAC、用途、状态、最后出现、走法）
tunlr devices name <设备> <名称>         起名
tunlr devices forget <设备>              删除记录（恢复为 proxy）
```

- `<设备>` 可以是 IP、MAC 或名称（不区分大小写）。列表中没有的私有地址可以预先设置；公网地址拒绝。
- 交互模式下省略 `<设备>` 时从列表（含 `--all` 的设备）选择，光标停在最近出现的设备上；省略走法时再选。
- 走法变化经 diff 确认后发布；只起名、不改配置时直接保存，不需要确认。
- 设置走法是 `tunlr filter device <设备> proxy|direct`（见 8.13）。`forget` 删除记录时直连的规则一并去掉，同样经 diff 确认。
- `status` 在网关或代理端口局域网访问开启时多一行 `LAN devices:`（在线、空闲的台数）；`--json` 全部支持。
- 设置走法放在 `tunlr filter` 而不是 `tunlr devices` 下（2026-10-28 讨论后改定）：用户按“我想让什么不走代理”找功能，设备直连与 QUIC、端口同属过滤（ShellCrash 的“流量过滤”中第一项也是过滤局域网设备）；`tunlr devices` 只负责查看、起名和删除记录。

### 8.13 流量过滤

`tunlr filter` 集中设置哪些流量不按订阅的规则走（2026-10-28 讨论后决定）。都由受管 mixin `tunlr:filter`（提供默认值，用户的 mixin 可以覆盖，或用 `tunlr.rules.delete` 去掉）在规则最前插入规则，变化经流水线发布（diff、确认、`mihomo -t`），失败时回滚，过滤项恢复原状。全部默认关闭，`init` 不询问。

| 项 | 规则 | 用途 |
|---|---|---|
| `device` | `SRC-IP-CIDR,<ip>/32,DIRECT` | 指定的局域网设备全部直连（设备与记录见 8.12） |
| `source` | `SRC-IP-CIDR,<网段>,DIRECT` | 来源在指定地址段的流量全部直连，如 Docker 网络、VPN 网段 |
| `quic` | `AND,((NETWORK,UDP),(DST-PORT,443)),REJECT` | 拒绝 QUIC，浏览器改用 TCP；很多节点转发 UDP 不稳定 |
| `ports` | `NOT,((DST-PORT,<端口>)),DIRECT` | 只代理常用端口，其余（BT、P2P、游戏）直连，不消耗节点流量 |

- 顺序即上表：直连的设备和地址段在最前，不受其他过滤项影响；QUIC 在端口之前，端口列表不含 443 时 QUIC 仍被拒绝。
- QUIC 不能只拒绝最终走代理的连接（规则匹配时还不知道结果），直连的网站也退回 TCP，影响很小。
- 默认端口沿用 ShellCrash：`22,80,143,194,443,465,587,853,993,995,5222,8080,8443`。可以改为端口和范围（`8000-9000`）的列表，由工具检查（内核不检查端口号，`99999`、`9000-8000` 都能通过 `-t`）；关闭时列表保留，与默认相同时记为默认。
- 规则写法在 tunlr-dev（v1.19.33）上用 `mihomo -t` 确认；端口以 `/` 分隔，`DST-PORT,22,80` 会把 80 当作出站名。
- 状态文件的 `filter`：`sources`、`quic`、`ports`、`port_list`。设备的走法仍在 `devices` 记录中。
- 在 tunlr-dev 上实测（开启透明代理，本机发起）：到 `www.google.com` 的 UDP 443 被拒绝（日志 `match AND(...) using REJECT`），HTTPS 照常；到 8888 端口的连接匹配 `NOT` 规则直连，443 照常按订阅的规则；把 8000-9000 加入列表后 8888 改为按订阅的规则走代理；全部关闭后规则和状态都已清理。
- 不做的：
  - 目标地址不进 TUN：mixin 写 `tun.route-exclude-address` 即可；做成开关容易把 DNS 服务器排除在外，使 DNS 劫持失效（8.2）。只想让某些地址直连时写 `IP-CIDR` 规则。
  - 中国 IP 直连：订阅的规则一般已有；在系统层绕过内核已经否决（8.12）。
  - 白名单模式（只有名单中的设备走代理）：暂不做，需要时用 `NOT,((SRC-IP-CIDR,…))` 实现。
  - 针对 Docker 的适配（检测网桥的网段、`exclude-interface`）：只做通用的 `source`，网段由用户填写（`docker network inspect` 可查）。在 tunlr-dev 上实测：开启透明代理时内核看到的容器连接来源就是容器地址（`172.17.0.2`，伪装在其后），`filter source add 172.17.0.0/16` 后容器到 8888 端口的连接由 `Match` 走代理变为 `SrcIPCIDR` 直连。`network_mode: host` 的容器算本机流量，不受影响；新建的 Docker 网络要另外设置。

**命令**：

```
tunlr filter                              列出各个过滤项
tunlr filter device <设备> proxy|direct   设置设备的走法（经 diff 确认后发布）
tunlr filter source                       列出直连的来源地址段
tunlr filter source add|remove <网段>     加入、去掉直连的来源地址段（经 diff 确认后发布）
tunlr filter quic on|off                  拒绝 QUIC
tunlr filter ports on|off                 只代理常用端口
tunlr filter ports set <端口>|default     更改只代理的端口
```

- 地址段写成网络地址加前缀长度（`172.17.0.0/16`）或单个地址（记为 `/32`、`/128`）。主机位不为零的（`172.17.0.9/16`）有歧义：可能想要整个网段，也可能只想要这个地址而多写了前缀，悄悄清零会让整个网段直连，所以拒绝，并给出两种写法。不接受 `/0`。
- 地址段只有“在名单中”一个状态，所以用 `add`、`remove` 增删（同 `sub add/remove`）；设备有自己的记录（名称、MAC），走法是它的属性，所以用 `device <设备> proxy|direct` 设置。
- 交互模式下省略设备、网段、走法、端口时询问（`source remove` 从名单中选）；非交互时缺少参数以退出码 2 报错。配置不变时（关闭时改端口列表）直接保存，不需要确认。
- 主菜单新增分类“Filters”（在“LAN devices”之后），右侧是摘要；`status` 多一行 `Filters:`，如 `1 device direct · QUIC blocked`，都关闭时为 `none`。

## 9. 从 ShellCrash 借鉴的功能

纳入：

1. **本机模式与网关模式**：二者需要的 sysctl、防火墙设置不同。（MVP 5，用 `tunlr tun gateway on/off` 切换，见 8.10；`init` 询问这台服务器的用途时可以一并开启，见第 10 节“家庭网络中的用法”）
2. **端口访问安全**：代理端口默认只给本机用；允许局域网访问（`tunlr proxy lan on`）时由 `lan-allowed-ips` 只放行私有地址段，其他来源必须设置认证，避免成为开放代理。（MVP 5，见第 6 节“端口安全”）
3. 流量过滤：`tunlr filter`（8.13），设备直连、来源地址段直连、拒绝 QUIC、只代理常用端口；不做绕过内核的系统层，目标地址不进 TUN 用 mixin。
4. 自定义保留地址段：用 mixin 写 `tun.route-exclude-address`（不是受管的键），已可用。
5. 开机联网检测与防自启死循环：已实现，`tunlr-core.service` 有 `After=/Wants=network-online.target`、`StartLimitBurst`、`OnFailure=tunlr-rescue.service`；开机时 `_prestart` 关闭过期仍未确认的 TUN（8.5）。
6. 失败通知：暂不做（见 3.4）。
7. 版本回退：已实现（内核的 `tunlr core rollback`、配置的 `tunlr config rollback`）。
8. 诊断与查看当前防火墙规则：已实现（`doctor`、`config plan --full`）。
9. 备份与还原：已实现（`tunlr backup`，见第 10 节）。

由内核能力替代：保存面板节点、规则集定时更新、NTP、订阅热更新（见第 6 节）。

不做：见 3.4。

## 10. 命令设计

命令树按领域组织，原则有三条（2026-10-29 整理，之前顶层平铺着 26 个命令）：

- **顶层只放领域名词和少数总览命令**，动作放在子命令里。顶层 16 个，`--help` 按下面的分组显示。
- **不带子命令时只查看，不做任何改动**（`config`、`settings`、`backup`、`tun`、`proxy`、`panel`、`devices`、`filter`、`update`、`sub`、`core` 都如此），因此不需要单独的 `list` 子命令。推论：查看命令上的参数只能改变看什么、怎么看（如 `devices --all`、`core logs -n`）；会改系统的都是子命令，动作也不靠参数变成查看或另一个动作。所以是 `panel reset-secret`、`core rollback`，配置历史由 `config` 显示，而不是 `panel --reset-secret`、`core upgrade --rollback`、`config rollback --list`。
- **几个词各司其职**：`config` 是 mihomo 的配置（合成、发布、mixin），`settings` 是工具自己的设置（`settings.yaml`），`update` 是订阅与 geo 数据的定时更新，`upgrade` 升级 tunlr 自身，`core upgrade` 升级内核。

```
Overview
tunlr status              当前状态（服务、版本、订阅、透明代理、漂移）
tunlr doctor              诊断（见 4.1；--offline 跳过网络检查）

Setup
tunlr init                首次安装向导；再次执行即修复
tunlr uninstall           完整卸载并清理规则（--purge 一并删除配置、订阅与状态）
tunlr upgrade             升级 tunlr 自身（只能手动执行，不做定时任务；待实现，实现前不注册，见 13.1）
tunlr --version           版本（没有 version 子命令；status 与各命令的 --json 中也有）

Configuration
tunlr config              当前修订与漂移，配置历史，mixin 文件和 tunlr 管理的 mixin（独占的键、修改它们的命令）
tunlr config plan         预览变更（--full 输出将写入系统的全部文件）
tunlr config apply        应用变更（默认先展示 diff，--yes 跳过确认；diff 超过 300 行时按段汇总，--full-diff 显示全部）
tunlr config rollback [号]   回滚配置（不给修订号时回到上一个不同的修订，交互时从历史中选）
tunlr config edit [名称]  用 $EDITOR 编辑 mixin.d 中的文件（<订阅名>/<文件> 为订阅 mixin），校验通过才保存，然后发布
tunlr sub                 已保存的订阅（可保存多个，同一时间只启用一个）
tunlr sub add <名称> <地址|文件|->   添加（--update 自动更新周期；--include、--exclude 生成订阅 mixin 中的节点筛选）
tunlr sub remove | update | use   删除、立即抓取（--source 换来源）、切换启用的订阅
tunlr update              定时更新：每个订阅和 geo 数据的周期、上次执行、下次到期
tunlr update run [sub|geo]   立即执行订阅、geo 数据的更新（不看周期；只抓某个订阅用 sub update）
tunlr update set sub [name] [period]   修改一个订阅的自动更新周期（6h、1d、off、default）
tunlr settings            工具设置（settings.yaml：GitHub 镜像、订阅的 User-Agent）
tunlr settings edit       用 $EDITOR 编辑设置，校验通过才保存，涉及配置时随即发布
tunlr backup              列出 backup restore 前留下的自动备份
tunlr backup create [-o 文件|-]   备份设置、mixin、订阅与状态中可迁移的部分（含凭据，0600）
tunlr backup restore [文件|-]     还原备份：diff 确认后发布，先自动备份当前内容；开关由用户选择是否打开；不给文件时从自动备份中选

Network
tunlr tun                 透明代理与网关模式的状态
tunlr tun on | off        透明代理（--mode 预留给将来的 tproxy）
tunlr tun gateway on | off   网关模式：局域网设备经本机分流（见 8.10；--interface 指定局域网网卡，交互时有多块候选就选择；on 时 TUN 关着就先开）
tunlr proxy               局域网设备要填的代理地址
tunlr proxy lan on | off  允许局域网设备使用代理端口（默认关闭，只放行私有地址）
tunlr proxy env [--unset] 当前 shell 的代理变量：输出 export 语句，eval "$(tunlr proxy env)"
tunlr panel               面板的 SSH 隧道命令与链接
tunlr panel reset-secret  换新的 API secret（diff 确认后发布，内核重启）
tunlr panel lan on | off  允许局域网设备直接打开面板（默认关闭）
tunlr devices             使用本机的局域网设备（--all 含未使用的）；name、forget 起名、删除记录（见 8.12）
tunlr filter              流量过滤：device、source 设备与地址段直连，quic 拒绝 QUIC，ports 只代理常用端口（见 8.13）

Kernel
tunlr core                内核的状态（服务、版本、已知可用版本）
tunlr core start | stop | restart | logs   服务控制（start、restart 后做健康检查；logs 透传 -f、-n）
tunlr core upgrade        升级内核到最新发布（--version 指定版本）
tunlr core rollback       换回上一个已知可用版本（不下载）
```

`completion`（cobra 自带的 shell 补全）可用但不显示在帮助中。

内部命令（供 systemd 调用，不显示在帮助中）：`tunlr _check`、`tunlr _rescue`、`tunlr _update`，以及透明代理用的 `_prestart`、`_poststop`、`_tun-guard`（见 8.4、8.5）。

服务与安装相关命令的行为：

- `init`：可重复执行，再次执行即修复。依次：检查环境（root、systemd、架构）→ 把自身复制到 `/usr/local/bin/tunlr` → 创建用户 `tunlr` → 建目录并设置属主与权限 → 没有内核时安装最新版 → 写入 unit 和 timer、`daemon-reload`、启用 `tunlr-core.service` 与 `tunlr-update.timer` → 给了订阅就添加 → 有启用的订阅时发布配置并启动内核、做健康检查。交互模式下询问 GitHub 镜像和订阅地址（都可以留空）；非交互模式由 `--mirror`、`--sub <名称>=<地址>` 给出，没给就跳过。
  - 之后询问这台服务器的用途（见下文“家庭网络中的用法”）：只给本机用（默认，如 VPS）、只给局域网设备当代理、给局域网设备当网关和代理（两种用法同时可用，选项文字写明，免得看起来是三选一）。后两项再问是否也让局域网设备打开面板（默认是）；选网关且有多块候选网卡（已启用、有 IPv4 地址，排除回环、TUN、docker/veth 等虚拟网卡，默认路由所在的排最前）时选择网卡；所选网卡有公网地址时再确认一次，回答否回到用途的问题。
  - 已经开启过透明代理、网关或代理端口局域网访问时不问（再次 init 是修复，不改用途）；没有订阅时也不问（内核不会启动）。
  - 非交互模式由 `--lan-proxy`、`--gateway <网卡|auto>`（同时开启 `--lan-proxy`）、`--lan-panel` 给出。它们与 `--json` 同用时报用法错误：后续步骤各有输出，合不成一份 JSON，分别执行 `proxy lan on`、`tun gateway on`、`panel lan on` 即可。
  - 这些步骤列在“Set up tunlr now?”的确认中，init 成功后依次走 `tun gateway on`（TUN 关着时先开 TUN）、`proxy lan on`、`panel lan on` 的流程，不再逐一确认（公网地址的确认已经在提问时做过）。某一步失败时报“tunlr is installed, but … failed”，退出码沿用那一步的（如 4 为已回滚），已经装好的部分不撤销。
  - 选“只给本机用”时，结果的最后提示 `tunlr tun on`、`tunlr proxy lan on`、`tunlr tun gateway on`。
- `core start`、`core stop`、`core restart`：执行 `systemctl`，`core start`、`core restart` 之后做健康检查。`core logs` 执行 `journalctl -u tunlr-core.service`，透传 `-f`、`-n`。
- `status`：服务状态、运行中的内核版本、已知可用版本、启用的订阅、最近一次发布、下次定时更新的时间。
  - 漂移（`pipeline.Drift`）：现场合成一次配置（不跑 `mihomo -t`）与生效的配置比较，报告四种情况：一致（不显示）；有尚未发布的改动（订阅更新、mixin 或设置改了、升级 tunlr 后模板变了、回滚到了旧修订），列出有变化的顶层键，提示 `tunlr config plan`；生效的配置与所有历史修订都不同，即在 tunlr 之外被改过，提示 `tunlr config apply` 会覆盖；无法合成（mixin 有错、端口安全检查不通过等），给出原因的第一行。还没有发布过时不报告。`--json` 中为 `drift`（`state`、`sections`、`error`），主菜单的状态行在修订号后提示。
  - 合成要解析整份订阅，主菜单每 10 秒刷新一次状态，所以按输入（状态、设置、订阅、mixin、生效的配置和历史）的大小和修改时间算出指纹（`DriftKey`），不变时沿用上次的结果。在 tunlr-dev 上 `status` 总耗时约 0.25 秒。
- `uninstall`：停止并禁用、删除 unit，删除 `bin/`、`core/`、`kernels/` 和用户 `tunlr`；保留 `/etc/tunlr`、订阅、历史、状态文件和 `/usr/local/bin/tunlr`（之后可以直接 `tunlr init` 恢复），`--purge` 才一并删除。需要确认（`--yes`）。
- `proxy env`：输出让当前 shell 使用代理端口的变量（子进程改不了父 shell 的环境变量，由用户 `eval`；同类做法如 `eval "$(ssh-agent)"`）。不做 `env on/off` 和 `shell-init`：要先在 shell 配置中装函数，且与 `tun on` 等持久开关同名而行为不同（只影响当前 shell）。
  - `http_proxy`、`https_proxy` 为 `http://127.0.0.1:<mixed-port>`，`all_proxy` 为 `socks5h://…`（域名交给代理解析），大小写各一份（curl 不认大写的 `HTTP_PROXY`，有的工具只认大写）；`no_proxy` 为本机与私有地址段（不认 CIDR 的工具忽略网段）。只输出 POSIX 写法，第一行是说明用法的注释。`--unset` 输出 `unset`，不需要配置。
  - 端口取自生效的配置；没有发布过、没有 `mixed-port`、代理端口需要认证时退出码 3。生效的配置含节点密码和 API secret，普通用户读不到，此时提示 `eval "$(sudo tunlr proxy env)"`：`sudo` 只生成语句，变量仍设在用户自己的 shell 中，工具因此不必为它放开 root 的约束。
  - 不做 `--lan`（局域网地址见 `tunlr proxy`）；透明代理开启时不提示（变量用不上但无害）。
- `config`：工具自己的设置（`/etc/tunlr/settings.yaml`：`github-mirror`、`user-agent`），与 mihomo 配置（mixin）分开。
  - `tunlr settings` 列出每项生效的值、是否为默认值和一句说明（何时生效），`--json` 中为 `file` 与 `settings`（`key`、`value`、`default`、`help`）。文件有错时报告错误。
  - `settings edit` 同 `config edit` 的流程：在临时副本上编辑（文件不存在时从带说明、默认值全部注释掉的模板开始）→ 解析校验（未知的键、取值不合法），不通过时可以回到编辑器或放弃 → 保存（编辑期间文件被改过时不覆盖，保留副本并给出位置）→ 合成配置，有变化（镜像）时 diff、确认、发布；还没有发布过时提示由 `config apply` 生效。不发布时设置仍保存，与 mixin 一致，`status` 的漂移会提示。`--dry-run` 只预览，不保存。需要终端，非交互时提示直接改文件再 `config apply`。
  - 不做 `config set/unset <key>`：只有两项，编辑器配合模板中的说明足够；不常改，命令越多越难记。geo 数据的更新频率在 mihomo 配置中，用 mixin 改。
- `backup`：与 `settings`、`config` 一致，不带子命令时只列出（自动备份），`create`、`restore` 才有动作。主要用于换服务器（VPS 重装、迁移），其次是同一台机器重装后恢复；只改配置时用 `config rollback`。
  - 不直接打包 `/etc/tunlr` 与 `/var/lib/tunlr`：状态文件中混有只对本机有意义的内容（实例 ID、已知可用的内核、TUN 的检测期限、网关开启前的转发设置），原样搬过去会出错。
  - 备份是一个 tar.gz（`pkg/core/backup`）：`backup.json`（格式版本、tunlr 版本、主机名、时间，以及状态中的订阅、启用的订阅、API secret、设备、过滤项和备份时开着的开关）、`settings.yaml`、`mixin.d/*.yaml` 与 `mixin.d/<订阅名>/*.yaml`、`subscriptions/<名称>.yaml`（最近一次抓取的内容，新机器暂时抓不到订阅也能立即发布）。不含内核、geo 数据、面板和配置历史（还原后要回滚的是还原前的配置，在本机历史中）。文件 0600，输出提示它含订阅地址与 API secret；不加密：要么引入依赖，要么自己实现，用户把文件放在哪里工具也管不了。
  - `backup create` 默认写到当前目录的 `tunlr-backup-<主机名>-<时间>.tar.gz`，已存在时不覆盖；`-o -` 写到标准输出，迁移可以一条命令完成：`ssh old tunlr backup create -o - | ssh new tunlr backup restore - --yes`（标准输入被占用，只能 `--yes`）。
  - `backup restore` 要求这台机器已经 `init`（有状态文件）。读取时检查格式版本（比当前新则拒绝）、文件名（只认上面几类，mixin 与订阅名称照常校验）、设置文件、订阅地址，以及启用的订阅有内容；错误信息不含订阅地址。
  - 设置文件、`mixin.d` 中的 `.yaml`（含订阅 mixin 的目录）与订阅内容整体替换（不合并，结果可以预料；原来的内容在自动备份中），状态只替换订阅、启用的订阅、API secret、设备和过滤项；实例 ID、内核、开关、定时任务的记录保持本机的。设备按地址记录，换了局域网就匹配不到，照样还原，diff 中可见。
  - 先按还原后的样子合成并校验（`pipeline.PlanRestore`，不写入任何东西），显示 diff，确认后在锁内：把当前内容自动备份到 `/var/lib/tunlr/backups`（保留 3 份）→ 写入 → 发布 → 同步 timer。全有或全无：发布失败时写回自动备份的内容，配置由流水线回滚，退出码 4；拒绝或预览时什么都不写。这一点与 `config edit`、`settings edit` 不同：还原只做一半，状态难以看明白。
  - 开关不自动打开：网卡、局域网、是否有公网地址都跟着机器，TUN 还有失联的风险。还原后比较备份与本机，备份时开着、这里关着的（网关的网卡在这里不存在时改用默认路由所在的网卡）在交互模式下列成多选、默认全选，选中的依次走 `tun gateway on`（或 `tun on`）、`proxy lan on`、`panel lan on` 的流程，同 `init`；非交互或 `--yes` 时只列出命令，`--json` 中为 `commands`。
  - 自动备份就是“撤销还原”的入口：`tunlr backup` 按时间列出（订阅、mixin 数和路径，`--json` 中为列表），`backup restore` 不给文件时在交互模式下从中选择；非交互时必须给出文件。
  - 主菜单有“Backup”分类：列出自动备份、备份到文件、还原自动备份（有自动备份时）、从文件还原（输入路径）。
- `sub use [名称]`：交互模式下询问是否立即发布，`--apply` 直接发布（同 `config apply`，非交互时需配合 `--yes`）。`--dry-run` 不切换，只按这个订阅被启用时的样子给出 plan。

**执行前确认**：会中断代理、替换内核、删除数据或丢掉用户刚做的修改的操作，执行前先确认，问题下方说明影响：

| 命令 | 问题 | 说明 |
|---|---|---|
| `config apply`、`config rollback`、`tun on/off`、`panel lan on/off`、`proxy lan on/off` | 是否发布 | diff 之后确认，下方给出节点变化 |
| `init` | Set up tunlr now? | 按现状列出将做的事（复制自身、建用户、装哪个内核、启用 unit、加订阅、发布、启动还是重启内核） |
| `core stop` | Stop mihomo? | 代理停止；TUN 开着时本机流量改为直连 |
| `core restart` | Restart mihomo? | 连接会断开几秒 |
| `core upgrade`、`core rollback` | Replace mihomo vX with vY? | 先查询目标版本（确认的是哪个就装哪个），已是该版本时不问；`rollback` 时给出将换回的已知可用版本 |
| `sub remove` | Remove the subscription …? | 列出来源（地址缩写），删除后只能用完整地址重新添加；有订阅 mixin 时说明一并删除几个文件 |
| `uninstall` | Uninstall tunlr …? | |
| `config edit`、`settings edit` 放弃修改 | Discard your edits? | 在“What now?”选择放弃或按 Esc 时再问一次，回答否回到选项 |
| `panel reset-secret` | Replace the API secret? | 同 `config apply`：diff（secret 隐去）之后确认；内核会重启，已打开的面板要用新链接重新连接 |

- 不确认的：`core start`、`sub add`、`sub update`、不带 `--apply` 的 `sub use`、`update run`、`update set`、只读命令和下划线开头的内部命令。它们要么只是新增、可以撤回，要么由 systemd 调用、不能提问；`update run` 做的就是定时任务会自动做的事。
- `--yes` 跳过确认。非交互模式下没有 `--yes` 时在开始工作前报用法错误（退出码 2）。`config apply` 等支持 `--dry-run` 的命令可以只预览；其余命令带 `--dry-run` 报用法错误，不把它当作已确认。
- 回答否或按 Esc 时输出一行（如 `Not stopped.`），退出码 0，主菜单的结果为 `– Cancelled`。已经做了一部分时（如 `config edit` 已保存 mixin、`settings edit` 已保存设置、`sub use` 已切换订阅）不发布不算取消。

**从已有条目中选择**：命令的参数是从已有条目中挑一个（或几个）时，交互模式下可以省略参数，改为在列表中用上下箭头选择、回车确定（多选时 x 或空格勾选），Esc 或 Ctrl-C 取消，取消不算失败。不提供 `/` 过滤：列表都不长，而过滤模式要占用 Esc（huh 默认只认 Ctrl-C 为取消，工具把 Esc 也加上）。非交互模式下省略参数的行为不变：原来必须给的照样报用法错误，原来有默认值的（如 `config rollback` 回到上一个不同的修订）照样用默认值，带 `--yes` 时也不询问。选项要能放进 80 列的终端，细节留给 `sub`、`config`。列表中每项附上做选择需要的信息，光标停在最可能的那一项上：

| 命令 | 列表 | 光标起始 |
|---|---|---|
| `sub use` | 全部订阅，标出启用的那个，附节点数、更新时间、流量与到期日 | 启用的订阅 |
| `sub remove` | 启用的以外的订阅（多选） | 第一项 |
| `config rollback` | 历史修订，附时间、来源、订阅，标出与生效配置相同的那一份 | 不带参数时会回到的那一份 |
| `config edit` | mixin 文件，末尾是“新建文件” | 第一项 |
| `tun gateway on` | 局域网网卡的候选（网关模式还没开启、多于一块时才问），附 IPv4 地址，标出默认路由与公网地址 | 默认路由所在的网卡 |

**主菜单**：在终端里只输入 `tunlr` 时进入主菜单；非交互模式（包括 `--json`）仍然显示帮助。菜单不是另一套界面，只是子命令的入口：每一项都对应一个命令，列表下方一行显示光标所在那一项的等价命令（`$ tunlr …`，订阅地址不含令牌；先填表单再执行的项显示参数的形式，如 `$ tunlr backup restore <file>`），用户在菜单里顺便学会命令。整个菜单期间只运行一个内联的 Bubble Tea 程序（不进入备用屏幕），列表和表单是嵌在里面的 huh 字段：

- 顶部一行状态：内核、启用的订阅、修订号、TUN，超出终端宽度的部分截掉。回到菜单时立即刷新，空闲时每 10 秒刷新一次（只查 systemctl、内核 API 和状态文件）；刷新失败时保留原来的状态并提示。刷新期间选中的操作等刷新完再执行，避免与读取状态的协程同时访问同一份数据。

- 两级。第一级依次是：需要处理的事项（没有时不显示）、“Show status”、分类、“Run diagnostics”（`doctor`）、“Quit”。分类与命令一一对应，按 `--help` 的分组（Configuration、Network、Kernel）排列，右侧显示当前状态：

  | 分类 | 命令 | 右侧状态 | 第二级 |
  |---|---|---|---|
  | Subscriptions | `sub` | 启用的订阅 · 共几个 | 列出、切换、更新、添加、删除 |
  | Configuration | `config` | 当前修订号（有没发布的改动、无法合成、在 tunlr 之外改过时标出） | 查看配置、历史与 mixin，预览，预览并发布，编辑 mixin，回滚 |
  | Scheduled updates | `update` | 订阅个数 · geo 数据的周期（失败的标红） | 查看、改一个订阅的周期、立即全部更新 |
  | Settings | `settings` | （无） | 查看、编辑工具设置 |
  | Backup | `backup` | 自动备份的份数 | 列出自动备份、备份到文件、还原自动备份（有时）、从文件还原 |
  | Transparent proxy | `tun` | 开 / 关 / 检查中，网关模式 | 开启或关闭 TUN、开启或关闭网关模式、查看详情 |
  | Proxy port | `proxy` | 局域网访问开 / 只有本机 | 查看地址、开启或关闭局域网访问 |
  | Panel | `panel` | 局域网访问开 / 只有本机 | 查看打开方式、开启或关闭局域网访问、更换 secret |
  | LAN devices | `devices` | 在线、空闲的设备数 | 列出、列出全部、起名、删除记录（8.12） |
  | Filters | `filter` | 摘要 | 见 8.13 |
  | Kernel (mihomo) | `core` | 运行中 · 版本 / 未运行 / 失败 | 查看，启动或重启、停止，最近的日志，升级，换回上一个已知可用版本（有可以换回的版本时），修复安装（`init`） |

  需要处理的事项以 `!` 开头，各对应一个命令，只有三种：有还没发布的改动（`config apply`）；内核服务失败或没有运行，且发布过配置（`core start`）；最近一次定时更新失败（`update run`，只有订阅或只有 geo 数据失败时带上 `sub` 或 `geo`）。只列“现在就该做、做什么很明确”的事：在 tunlr 之外改过的配置要用户先看 diff 再决定，留在 Configuration 分类里；没有订阅、没发布过配置时状态行已经说明。光标起始于第一项，有事项时就停在它上面。

  第二级末尾是“← Back”。没安装时第一级只有“Install tunlr (init)”“Show status”“Run diagnostics”“Quit”。菜单项随状态变化：有订阅才有切换、更新；订阅多于一个才有删除；有历史才有回滚；开关类的项只出现与当前状态相反的那个动作。
- 第一级尽量一屏放下：列表的高度按终端的行数算（减去状态行、标题、按键说明和等价命令），至少 10 行，放不下时滚动；终端大小变化时随之调整，光标不动。
- 不加分组标题，也不自己写列表组件：分组从顺序上看得出来（与 `--help` 一致），huh 的 Select 没有不可选的行，为几行标题自己写列表（光标跳过标题、滚动、按键）得不偿失。
- 不清屏。在两级之间切换只是原地重画；列表标题是所在位置和这个分类在第一级右侧的状态（`tunlr › Transparent proxy · off`），光标停在上次在这一级选的那一项。
- 选中操作后，菜单那块换成一行操作名和等价命令（订阅地址不含令牌），终端经 `tea.Exec` 交给子命令（Bubble Tea 先等读取输入的协程退出），子命令照常输出和提问，vim、less 也照常使用。结束后输出一行结果（`✓ Done`、`✗ Failed`、`✗ Failed; the change was rolled back`、`✗ Check failed`、`– Interrupted`，带耗时），菜单紧接着出现在结果下面，光标停在刚才那一项。结果就在菜单正上方，不必再按回车确认；所有输出都留在滚动记录里，退出菜单时菜单本身不留在屏幕上。提问被取消或确认时回答否（什么都没做）时结果为 `– Cancelled`，不算失败。
- 按键规则处处相同：任何提问（列表、输入、确认）按 Esc 或 Ctrl-C 取消，回到上一级；在第一级按 Esc 退出菜单。列表不需要输入文字，`q` 也能取消（与 less 一致）。
- 子命令里的提问是菜单交出终端期间另起的 huh 程序。取消的提问必须像提交一样正常退出。huh 在用户取消时发送 `tea.Interrupt`，Bubble Tea（v2.1.0 仍是如此）把中断当作程序被杀死，不等读取输入的协程退出就返回；紧接着运行的下一个提问或菜单与这个残留的协程争抢输入，按键时而被它读走丢掉。在 tunlr-dev 上约一半的菜单流程因此丢键，曾把方向键丢掉而误执行了“停止内核”。所以每个表单都带一个 `tea.WithFilter`：表单已标记为取消时，把它发出的中断换成正常退出，`Run` 照样返回 `huh.ErrUserAborted`；不是用户取消的中断保持原样。不要用每次打开 `/dev/tty` 的办法绕开。操作执行中按 Ctrl-C 只中断这一个操作（照常走事务的回滚），结果为 `– Interrupted`，不会退出菜单。
- 添加订阅时名称和来源（地址或文件路径）在菜单里的同一个表单中，可以用 Shift-Tab 回到名称修改，两项都当场校验；Esc 回到列表。
- 不放 `uninstall`：卸载不该是在菜单里顺手按到的操作。
- 子命令失败不退出菜单：错误照常报告，结果行显示失败。

**输出样式**：标准输出是终端、且没有设置 `NO_COLOR` 或 `TERM=dumb` 时，用 lipgloss 加样式，只用 16 色：

- 耗时的步骤（合成与校验、发布、抓取订阅、启动与升级内核、`init` 与 `tun on` 的各步）前显示 spinner，完成后换成 ✓ 或 ✗ 加耗时。步骤进行中的日志经由同一个输出写在 spinner 上方，不会和它挤在一行。
- 下载内核、geo 数据时，spinner 的那一行带进度条、已下载/总大小和速度（用 bubbles `progress` 的 `ViewAs` 只渲染字符串，不起 Bubble Tea 程序）。进度经 context 传给 `pkg/core/download`（`download.WithProgress`），core 不依赖任何界面库。
- diff 中增加的行绿色、删除的行红色、段头青色；检查结果用 ✓ ✗ –；状态值按好坏上色。
- 长输出（`config plan`、`config apply` 的计划与 diff，`core logs`）超过一屏时像 git 一样交给分页器：`$PAGER`，默认 `less`，未设置 `LESS` 时用 `FRX`（不足一屏直接输出、保留颜色、退出后内容留在屏幕上）。只在标准输入输出都是终端时分页。
- 确认发布时，问题下方给出节点变化的一行摘要，不必回头翻 diff。
- huh 的提问用 `ThemeBase16`，跟随终端的 16 色，与其余输出一致。
- 其他情况（管道、重定向、systemd 运行的内部命令、`--json`）输出与不加样式时逐字相同：步骤为 `==> 步骤`，结果为 `ok`、`failed`、`skipped` 文字列，脚本照常解析。

**面板（`tunlr panel`）**：运行时交互全部交给 zashboard，但面板只监听服务器的 `127.0.0.1`，API 的 secret 由 tunlr 生成，用户无从得知。`tunlr panel` 一次给出打开面板的全部步骤：

- 从**生效的配置**读取 `external-controller` 和 `secret`（mixin 改过端口或 secret 时也对得上），不读状态文件。没有发布过配置、`external-controller` 为空时说明面板不可用。
- 输出在用户电脑上执行的 SSH 隧道命令：`ssh -N -L <端口>:127.0.0.1:<端口> [-p <SSH 端口>] <用户>@<服务器>`，两端都用 API 的端口（默认 9090）。服务器地址和 SSH 端口取自 `$SSH_CONNECTION` 中本机的一端，用户取 `$SUDO_USER`，没有则取 `$USER`；不在 SSH 会话中时写成 `<server>`。经别名、跳板机或 NAT 连接时这个地址不对，所以总附上一句“用平时连接的主机名”，以及 `~/.ssh/config` 中 `LocalForward` 的一次性写法。
- 输出浏览器中打开的链接：`http://127.0.0.1:<端口>/ui/#/setup?hostname=127.0.0.1&label=<主机名> (SSH tunnel)&port=<端口>&secret=<secret>`（zashboard README 中的写法），打开即自动连上后端。secret 默认完整显示：命令需要 root，这正是用户要的东西。
  - 模板写 `external-ui: ui/zashboard`，不用 `external-ui-name`：内核把 `external-ui` 指向的目录挂在 `/ui`，`external-ui-name` 只决定把下载解压到其中的哪个子目录，用它时 `/ui/` 是一个目录列表，要再点一级。没有 `external-ui-name` 时内核直接解压到 `external-ui`（读源码 `component/updater/update_ui.go`：zip 里唯一的顶层目录 `dist/` 被展开），面板中的“更新面板”照常工作。
  - `label` 是 zashboard 后端列表中的名称，取主机名，经隧道的另加 `(SSH tunnel)`。zashboard 的 `addBackend` 只在各项（含 label、secret）完全相同时复用已有的后端，否则新增一个，所以 label 保持固定，重复打开链接不会多出后端；`panel reset-secret` 之后旧的后端留在浏览器中，提示用户去面板的设置里删除。
  - 后端存在浏览器的 localStorage 中，第一次用链接打开之后直接打开 `http://<地址>:<端口>/ui/` 即可，输出中一并给出这个地址。
  - zashboard 可以从 `./zashboard-settings.json`（键为 localStorage 的键）或内核的 `/storage/zashboard` 导入设置，但都要用户先在浏览器里打开“自动导入”或手动导入；暂不预置，等有确定要预置的设置再做。
- API 不只监听本机时（局域网访问开启，或配置在工具之外被改过），另外给出直接打开的链接：监听所有地址时，取默认路由网卡的 IPv4 地址，以及 SSH 会话进来的地址（如 Tailscale）。隧道照样列出，在外网时用。
- 显示面板是否已安装（`external-ui` 目录）、内核是否在运行，不满足时给出 `tunlr config apply`、`tunlr core start` 的提示。
- 不使用 zashboard 的 `disableTunMode`、`disableUpgradeCore` 参数：不限制面板能力（见第 3 节）。
- `tunlr panel reset-secret`：生成新的 secret 并发布（diff、确认，同 `config apply`；`secret` 需要重启内核才生效）。`secret` 归 `tunlr:panel`，mixin 不能设置。用于 secret 可能泄漏时。
- 主菜单的分类“Panel”：查看打开方式、开启或关闭局域网访问、更换 secret；`init` 成功后最后一行提示 `tunlr panel`。

**面板的局域网访问（`tunlr panel lan on | off`）**：家庭网络中，其他设备（手机、平板）要能直接打开面板调整，不便每台都建 SSH 隧道。默认关闭。

- 开关存在状态文件中（`panel_lan`），像透明代理开关一样作为合成的输入：`tunlr:panel` 中写 `external-controller: 0.0.0.0:9090`（关闭时写 `127.0.0.1:9090`）（实测内核按 `*:9090` 双栈监听，所以 nft 表也限制 IPv6 来源；链接只给 IPv4 地址）。经流水线发布：diff、确认、`mihomo -t`；`external-controller` 需要重启内核才生效，失败时回滚，开关恢复原状。
- `external-controller` 归 `tunlr:panel`，mixin 设置了它时合成就会拒绝，提示改用 `tunlr panel lan on/off`。面板经明文 HTTP 访问，secret 在局域网中可见，`lan on` 时说明。
- 内核 API 不能限制来源，只监听 `0.0.0.0` 在公网 VPS 上等于把面板交给整个互联网。所以开关开启时另外加载 nft 表 `inet tunlr_panel`：input 链（优先级 `filter - 1`，策略 accept）中，目标为 API 端口、来源不在私有地址段（`127.0.0.0/8`、`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`100.64.0.0/10`（含 Tailscale）、`169.254.0.0/16`；IPv6 为 `::1`、`fe80::/10`、`fc00::/7`）的 TCP 包丢弃。在 tunlr-dev 上实测：从私有地址访问返回 200，从临时加在 lo 上的 `198.51.100.77` 访问超时。
  - 先加载表再发布配置，关闭时先发布再删表，API 对外的任何时刻都有这层保护。
  - 规则不跨重启保存：`_prestart` 在内核启动前补上；`_check` 只随透明代理运行，所以 `nft flush ruleset` 之后要到下次启动内核才补上，`doctor` 和 `tunlr panel` 会指出缺失。`_poststop` 保留它。`uninstall` 删除。
  - 这张表只能多挡一层，不能放行：机器上其他防火墙挡了这个端口时，它照样打不开，`lan on` 完成后提示检查已有的防火墙。
- 默认路由网卡的 IPv4 地址不是私有地址时（服务器很可能直接在公网上），`lan on` 先给出警告，再单独确认一次（`--yes` 时两次都视为同意）。nft 表仍然只放行私有地址段。
- 回滚、自动关闭透明代理改写独占的键时，`external-controller` 和 `secret` 也按当前开关写入：回滚到开启前的修订，面板照样对局域网开放，与 nft 表一致。

**家庭网络中的用法**：家里一台 Linux 服务器运行代理，其他设备有两种用法，`init` 的用途问题对应这两种（以及只给本机用）：

| 用法 | 设备上的设置 | 工具做的事 | 覆盖范围 |
|---|---|---|---|
| 代理 | Wi-Fi 设置中把 HTTP/SOCKS5 代理设为 `服务器:7890` | `proxy lan on` | 遵守系统代理的应用 |
| 网关 | 网关和 DNS 设为服务器（手动，或在路由器的 DHCP 中） | `tun on` + `tun gateway on`（8.10），并 `proxy lan on` | 设备的全部流量；同时也能当代理用 |

网关模式下也开启代理端口：有的设备不方便改网关（或只想让个别应用走代理），两种用法可以并存。

**代理端口的局域网访问（`tunlr proxy lan on | off`）**：

- 开关存在状态文件中（`proxy_lan`），由独占的受管 mixin `tunlr:proxy` 写成配置：`allow-lan` 按开关写 `true` 或 `false`；`lan-allowed-ips` 总是写成私有地址段（`config.LANAllowedIPs`：`127.0.0.0/8`、`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`100.64.0.0/10`、`169.254.0.0/16`、`::1/128`、`fc00::/7`、`fe80::/10`）。模板中也写了这份列表，紧跟 `allow-lan`，让它在配置中的位置固定、开关的 diff 只有一行（不放在模板里时合并会把它追加到配置末尾的规则之后）。mixin 设置这两个键时合成拒绝，提示改用 `tunlr proxy lan on/off`；要密码时在 mixin 中写 `authentication`。
- 来源由内核限制，不需要 nft 表（与面板不同：API 不能限制来源）。在 tunlr-dev（v1.19.33）上实测：没有 `lan-allowed-ips` 时放行所有来源；只写私有地址段时局域网来源可用，临时加在 lo 上的 `198.51.100.77` 被拒绝。回环地址也受这个列表限制，所以必须包含 `127.0.0.0/8` 和 `::1/128`。`::1/128` 在 YAML 流式列表中要加引号，所以写成块列表。
- `allow-lan` 热重载即生效（内核重新绑定 `*:7890`），不重启内核。经流水线发布：diff、确认、`mihomo -t`，失败时回滚，开关恢复原状。
- 默认路由网卡的 IPv4 地址是公网地址时，`lan on` 先警告，再单独确认一次。
- `tunlr proxy` 显示开关和局域网设备要填的地址（默认路由网卡的 IPv4 地址加 `mixed-port`）；`status`、`doctor`（检查生效配置与开关是否一致）、主菜单的分类“Proxy port”中也有。

设计思路：高频的服务控制放在顶层，与 `systemctl` 的直觉一致；只保留 `sub`、`tun`、`env` 三组子命令（`sub use` 的说法与 `kubectl config use-context`、`nvm use` 一致）；合并含义重叠的命令。检验标准：没看过文档的人能猜对大部分常用操作。

全局参数、非交互模式与退出码见 `CLAUDE.md`。

## 11. 为将来预留

v1 只做 CLI，但以下几点从第一版起就做好，避免以后返工：

1. 所有命令支持 `--json`，输出带版本号；退出码有明确定义
2. 配置和状态文件带 schema 版本号，结构变化时写迁移
3. 每个安装实例生成唯一 ID（将来多服务器管理时用于识别）
4. 核心逻辑放在公开的 `pkg/core`，可被其他 Go 组件复用
5. 不 import mihomo 代码，保证开源协议的选择权在自己手里

**将来的 agent**：桌面端、手机端和 Web 控制台需要远程管理服务器时，需要一个常驻的 agent 接收请求。计划作为同一二进制的子命令 `tunlr agent` 放在本仓库，由 systemd 托管、默认不开启，与 CLI 共用 `pkg/core`。届时必须单独设计身份认证、传输加密和权限控制。

**开源协议**：待定。因为不链接 mihomo 代码，不受其 GPL-3.0 约束。一种常见做法是 CLI 使用 MIT 或 Apache-2.0，托管服务和控制台另行决定。

## 12. 分发

- 安装入口：`curl -fsSL https://tunlr.app/install.sh | sh`。脚本只识别架构、下载、校验 checksum，然后调用 `tunlr init`；支持指定镜像源或代理，缓解国内服务器访问 GitHub 慢的问题；无论经哪个镜像下载，都按官方 sha256 校验（见第 7 节）。
- `.app` 域名强制 HTTPS，托管时需配置证书（Cloudflare 或 GitHub Pages 会自动处理）。
- 可审计性：`tunlr config plan --full` 输出将写入系统的全部内容（mihomo 配置、nft 脚本、systemd unit），用户执行前即可审查。
- 名字的读音和来历写在 README 与官网首页；提供 shell 自动补全。

## 13. MVP 顺序

1. 配置流水线：模板 + mixin → 校验 → 原子写入 → 重载 → 失败回滚
2. systemd 托管与内核安装、升级、已知可用版本
3. 透明代理开关（TUN + auto-redirect），带自动检测与超时回滚
4. `doctor` 诊断
5. 本机/网关模式、端口安全（ShellCrash 借鉴项 1、2）

其余功能在此之后按需求排期。

进度（2026-10-28）：1～5 已完成；此外完成了主菜单、执行前确认、定时任务治理（第 7 节）、面板（`tunlr panel`，含局域网访问）、受管 mixin（第 6 节），以及家庭网络的用法：代理端口的局域网访问（`tunlr proxy lan`）和 `init` 的用途问题（第 10 节），`status` 的漂移（第 10 节），局域网设备的列表与分流（`tunlr devices`，8.12），流量过滤（`tunlr filter`，8.13），代理变量（`tunlr proxy env`，第 10 节），工具设置（`tunlr settings`，第 10 节），备份与还原（`tunlr backup`，第 10 节）。

### 13.1 待实现（按顺序）

动手前先讨论用户可见的行为；“设计”一列为“已定”的可以直接实现，其余先补全设计再动手。

| 顺序 | 项目 | 出处 | 设计 | 要点 |
|---|---|---|---|---|
| 1 | ~~`status` 的漂移~~ | 第 10 节 | 已完成 | 见第 10 节 `status` |
| 2 | ~~局域网设备的列表与分流~~ | 8.12 | 已完成 | 见 8.12；内核没有按 MAC 的规则，按地址匹配 |
| 3 | ~~流量过滤~~ | 8.13 | 已完成 | 设备直连（从 `devices set` 移来）、来源地址段直连（含容器网段）、拒绝 QUIC、只代理常用端口；目标地址不进 TUN 用 mixin |
| 4 | ~~代理变量~~ | 第 10 节 | 已完成 | `tunlr proxy env [--unset]` 输出 `export`/`unset` 语句，用法 `eval "$(tunlr proxy env)"`；不做 `shell-init`、`env on/off` |
| 5 | ~~工具设置~~ | 第 10 节 | 已完成 | `tunlr settings` 列出设置，`settings edit` 在编辑器中修改；不做 `set/unset` |
| 6 | ~~备份与还原~~ | 第 10 节 | 已完成 | 见第 10 节 `backup` |
| 6a | ~~命令树整理~~ | 第 10 节 | 已完成 | 顶层 26 → 15（`upgrade` 实现后为 16）：`core`（start/stop/restart/logs/upgrade）、`config`（plan/apply/rollback/edit，含原 mixin）、`config` 改名 `settings`、`gateway` 并入 `tun`、`update` 移到 Configuration（不带子命令只查看，立即执行改为 `update run`，`sub set` 并入 `update set sub`）、去掉 `version` 子命令、隐藏 `completion`；帮助按分组显示；旧命令全部删除、不留别名；提示文字、主菜单、测试、本文档同步。随后按“查看命令的参数只改变看什么”的推论：`panel reset-secret`、`core rollback` 改为子命令，`config` 显示配置历史（去掉 `rollback --list`）；`tun gateway on` 交互时选局域网网卡；主菜单按帮助分组重排（Settings 独立成分类、最上面是需要处理的事项、底部显示等价命令、第一级一屏放下、Kernel 分类加回滚和修复安装） |
| 6b | ~~mixin 分为 `mihomo:` 与 `tunlr:` 两个命名空间~~ | 第 6 节 | 已完成 | `mihomo:` 原样合并（映射合并、标量和列表替换、`null` 删除），`tunlr:` 下按 `proxies`、`proxy-groups`、`rules`、`sub-rules.<名称>` 分组提供 `prepend`/`append`/`patch`/`delete`，`patch` 项内的列表字段用 `prepend-<字段>` 等前缀。受管 mixin、`templates/mixin.yaml`、`sub add` 生成的筛选文件、提示文字、golden 文件同步 |
| 7 | `tunlr upgrade`（自身升级） | 第 10 节 | 待补全 | 正式发布前必须有：查询最新版本、下载（可经镜像，只认官方 sha256）、用新文件跑 `--version` 确认、原子替换 `/usr/local/bin/tunlr`、必要时重写 unit 和 timer。待定：是否保留旧版本以便回退、开发版如何处理。不做定时任务 |
| 8 | 发布 | 第 12 节 | 待补全 | `.goreleaser.yaml`（amd64、arm64，带 checksum）还没跑过真正的发布，没有 CI；`install.sh` 与官网未做 |

不做或暂不做：失败通知（3.4）；局域网设备绕过内核的系统层（8.12）。`completion` 由 cobra 自动提供。

另有草案，位置待定：常用网站的连通性测试（暂名 `tunlr probe`）。经代理端口按真实分流规则请求一组常用网站（国内、海外、AI、流媒体各几个，可在设置中替换），报告是否连通、耗时，并经 `/connections` 按源端口找到这条连接，给出匹配的规则和链路；只在用户执行时出网，不在 `status` 和菜单刷新中进行。代替“在 status 中显示出口 IP 或当前节点”：分流下各网站的出口不同，单个 IP 或节点会误导（2026-10-28 讨论）。

## 14. 待决事项

- 开源协议
- 商标查询
