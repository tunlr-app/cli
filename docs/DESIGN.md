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
- 诊断、救援、失败通知、备份还原、当前 shell 代理变量

### 3.3 灰色地带的约定

原则：**不限制面板能力，工具做兜底**。内核支持的操作，面板照样能用。

- **TUN 开关**：面板可以在运行时切换，但不会写回配置文件，也不处理系统前置条件。持久化的开关以工具为准；工具读取实际状态，并提供“把当前运行状态保存到配置”的操作。
- **内核升级**：面板可以通过内核自带接口升级；工具负责兜底——保留已知可用版本，内核启动失败时自动回滚，并在定时任务中核对面板升级来的二进制（见第 7 节）。工具自己升级时会先做兼容性校验，没有断网窗口。
- **重载配置**：面板重载读的是工具生成的文件，不冲突。
- **geo 更新**：定时更新由工具的 `_update` 触发内核更新并核对结果（见第 7 节），面板手动刷新作为补充。
- **订阅更新**：订阅是完整配置，由工具的 timer（`tunlr _update`）拉取并走完整的配置流水线；配置中的 rule-providers 等仍由内核按 `interval` 刷新。

### 3.4 明确不做

- 全屏 TUI、常驻守护进程（v1）
- sing-box 等其他内核、路由器固件、OpenRC/procd、WSL、iptables、容器化部署
- Telegram 机器人、公网入站、内网穿透、DDNS
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

## 5. 技术选型

### 5.1 为什么是 Go 单文件，而不是 sh

| | 纯 sh | Go 单文件 |
|---|---|---|
| 依赖 | curl、yq、jq、shell 方言 | 只依赖 `nft` 和 systemd |
| 配置处理 | 文本替换，易出错 | 结构化 YAML 节点 |
| 错误处理与回滚 | 难写对 | 事务模式 + 单元测试 |
| 体积 | 几百 KB | 约 8–12 MB（去调试信息后，待实测） |

sh 的优势（轻量、随处可跑、可随手修改）主要服务于路由器和老旧固件，而本项目已经放弃这些平台。sh 只保留在两处：安装脚本（识别架构、下载、校验后调用 `tunlr` 完成安装）和 `shell-init` 输出的 shell 函数（子进程无法修改父 shell 的环境变量）。

减小体积：`-ldflags="-s -w"`、`-trimpath`，另提供可选的 upx 压缩版本。也评估过 Rust（可小约 5 MB），但相对 17–19 MB 的内核压缩包意义不大，且 Go 的 `yaml.Node` 对“保留顺序和注释的合并”支持更好，生态也与 mihomo 一致。

### 5.2 为什么不做 TUI

逐项核对功能清单后，没有任何一项必须用全屏界面：安装向导、确认、多选都能用 `huh` 在普通终端完成；状态用 `lipgloss` 渲染，配合 `--watch` 刷新；实时日志和流量交给 zashboard 或 `journalctl -f`。注意 `huh` 内部基于 Bubble Tea，所以它仍会作为间接依赖存在，但项目代码不写 Bubble Tea 程序。

### 5.3 为什么不常驻

| 需求 | 实现 |
|---|---|
| 定时更新 | systemd timer 调用 `tunlr _update` |
| 内核崩溃后恢复与回滚 | unit 中配置 `OnFailure=`，触发 `tunlr _rescue` |
| 开启 TUN 后的防失联回滚 | `systemd-run --on-active=<时长>` 创建临时回滚定时器 |
| 健康检查、规则丢失检测 | timer 周期调用 `tunlr _check` |
| 失败通知 | 在上述一次性任务中发送 webhook |
| 状态与漂移 | 执行 `status` 时现场计算 |

好处：零常驻内存、更少的故障点、升级工具只需替换文件、日志由 journald 按 unit 归档。

### 5.4 依赖清单

见 `CLAUDE.md`。补充两点：

- **YAML 合并基于 `yaml.Node`**：保留键顺序和注释，生成的配置可读、diff 干净；不需要为 mihomo 的大量字段定义结构体。
- **nftables 首选 `nft -f`**：规则用 nft 原生语法写成模板，生成的脚本落盘可读、可手动执行、易于 golden 测试；`nft -f` 整个文件在一个事务中提交。`google/nftables` 只用于程序化查询（如检查表是否存在）。

### 5.5 磁盘布局

| 路径 | 内容 | 说明 |
|---|---|---|
| `/etc/tunlr/` | mixin（`mixin.d/*.yaml`，目录 0700、文件 0600）、工具设置（`settings.yaml`） | 用户可编辑，备份时最需要保留 |
| `/var/lib/tunlr/state.json` | 状态文件：结构版本号、安装实例 ID | 工具维护，权限 0600 |
| `/var/lib/tunlr/subscriptions/` | 每个订阅最近一次成功抓取的内容 | 含机场凭据 |
| `/var/lib/tunlr/history/` | 已发布配置的历史：每个修订一份配置（`<号>.yaml`）和一份记录（`<号>.json`） | 用于回滚，保留最近 10 个，权限 0600 |
| `/var/lib/tunlr/kernels/<版本>/mihomo` | 已知可用的内核 | root 所有，保留最近 3 个，见第 7 节 |
| `/var/lib/tunlr/bin/mihomo` | 运行中的内核，也用于校验配置 | 归内核用户 `tunlr` 所有，面板升级时原地替换 |
| `/var/lib/tunlr/core/` | mihomo 工作目录（`-d`）：生效的配置、API socket（`api.sock`）、geo 数据、面板、缓存 | 归内核用户 `tunlr` 所有，权限 0750（保护 API socket） |
| `/etc/systemd/system/tunlr-*.{service,timer}` | 工具生成的 unit：`tunlr-core.service`、`tunlr-rescue.service`、`tunlr-update.service`、`tunlr-update.timer`，MVP 3 起还有 `tunlr-check.service`、`tunlr-check.timer` | 每次 `init` 重写 |
| `/usr/local/bin/tunlr` | 工具自身 | unit 引用这个路径，`init` 时不在这里则复制过来 |
| `/run/lock/tunlr.lock` | 文件锁 | 放在 `/run` 下，安装前也可用 |

- `/var/lib/tunlr` 为 0711：内核用户只能进入它拥有的 `bin/`、`core/`，不能列出或读取其他内容（订阅、历史、状态文件都是 root 的 0600/0700）。

- 所有路径由 `pkg/core/paths.Layout` 统一给出，测试时设置 `Root` 把整套布局放进临时目录。这样的测试布局不涉及内核用户：文件不改属主，`mihomo -t` 以当前身份运行。
- 文件一律经 `pkg/core/fsutil.WriteFile` 原子写入：同目录临时文件 → 刷盘 → `os.Rename` → 刷新目录。
- 锁被占用时重试到期限为止，超时报告“另一个 tunlr 操作正在进行”。锁文件不删除，否则并发的获取者会锁住不同的文件。
- 状态文件读取时检查版本：比当前工具新则拒绝并提示升级，比当前旧则按迁移链逐级升级。
- 工具设置（`pkg/core/settings`）由用户编辑，文件不存在时用默认值；未知的键视为错误，避免拼写错误被静默忽略。目前的设置项：`github-mirror`（GitHub 下载镜像前缀，只支持一个；作用见第 6 节“内核自己的下载”）、`user-agent`（抓取订阅的默认 User-Agent）。

## 6. 配置流水线

```
模板 + 订阅（机场完整配置）+ 用户 mixin + 工具强制项
        → 合成最终配置
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
  - mixin 对所有订阅共用。它可能引用某个机场特有的策略组：`patch-`、`delete-` 找不到目标时只给出警告；插入了不存在的名称等导致 `mihomo -t` 不通过时，不发布并提示原因。
  - 不能删除正在启用的订阅，需先切换到其他订阅。
- **订阅抓取**（`pkg/core/subscription`）：
  - 机场按 User-Agent 决定返回的格式。默认 `clash.meta mihomo tunlr/<版本>`，同时命中面板常见的 `clash.meta` 与 `mihomo` 子串判断；返回 base64 节点列表时，报错提示更换 User-Agent。可以自定义，优先级从高到低：
    - 单个订阅自己的 User-Agent：`sub add --user-agent` 指定，已有订阅用 `sub update <name> --user-agent` 修改（只在抓取成功时保存，试错不会丢掉原来可用的值；`--user-agent ""` 恢复默认）。
    - `settings.yaml` 中的 `user-agent`，作用于所有没有自己 User-Agent 的订阅。
    - 内置默认值。
  - User-Agent 不能含控制字符或首尾空白，避免 HTTP 头注入。
  - 内容必须是 YAML 映射且带有节点（`proxies` 或 `proxy-providers`），否则视为抓取失败。响应限制 32 MB、60 秒。
  - 读取 `subscription-userinfo` 响应头，记录流量与到期时间，供 `sub list` 显示和将来的到期提醒。
  - 订阅 URL 通常带有访问令牌：输出、日志和错误信息中只出现 `scheme://host/...`，net/http 错误中附带的完整 URL 会被去掉。
  - 订阅列表和抓取信息存在状态文件中，内容存在 `subscriptions/<名称>.yaml`（目录 0700，文件 0600）。添加失败时不留下任何文件；内容未变时不重写，减少刷盘（测试机的虚拟磁盘每次刷盘 35～140 毫秒）。
  - 只含节点的订阅（走 proxy-providers、由模板提供策略组和规则）留作将来的第二种来源，与完整配置共用同一条流水线。
- **分层合成，各层职责固定**（`pkg/core/config.Compose`）：
  1. 模板：工具内置的系统部分，如端口、DNS、geo 更新与下载地址、面板、profile。
  2. 订阅：**只取路由部分**，即 `proxies`、`proxy-groups`、`rules`、`proxy-providers`、`rule-providers`、`sub-rules`，其余键一律丢弃并在 `plan` 中列出。订阅是不可信输入，不能让机场配置打开端口、改监听地址或设置 API。
  3. 工具生成的调整：工具为这台服务器生成的值，在内存中写成一份名为 `tunlr` 的只读 mixin，以 mixin 的语义应用，用户的 mixin 可以覆盖或去掉（覆盖记录显示为“覆盖 tunlr”）。包括 HTTP API 的 `secret`（第一次发布时随机生成，存在状态文件中），以及配置了 `github-mirror` 时经镜像的面板地址（`external-ui-url`）、geo 数据地址（`geox-url`）和让镜像主机直连的规则（见下文“内核自己的下载”）。`plan` 列出这一层的全部内容（`secret` 的值隐去），用户由此知道哪些键由工具设置、该怎样覆盖。
     - 不写入 `mixin.d`：写成文件会让用户以为可以编辑它，改了又被工具覆盖；镜像设置变化时还要同步文件。
     - 放在订阅之后而不是紧接模板：插入规则需要订阅的 `rules` 已经就位；这一层的键模板中都有（`geox-url` 也写在模板里，地址与工具校验下载的 geo 数据一致），合并后位置不变，输出稳定。
  4. mixin：用户的覆盖，可以改任何键（强制项除外），见下文。
  5. 工具强制项：工具运行所依赖的键（`external-controller-unix`，以及透明代理开关决定的几个 `tun` 键，见 8.3），最后写入，mixin 无法覆盖。
- **mixin**（`pkg/core/config/mixin.go`）。参考了 clashctl（单个 mixin.yaml，嵌套的 `prepend`/`append`/`override`/`inject`）和 ShellCrash（按类别分文件、文本拼接、`-t` 失败时静默丢弃全部自定义）后的设计：
  - 场景：改通用设置；加自己的节点、策略组、规则、rule-providers、listeners；改订阅里已有的策略组和节点（塞入自己的节点、改 interval、加 dialer-proxy）；去掉机场的信息节点和不要的组或规则；按用途分文件、可以单独停用。
  - **语法：mihomo 原生写法 + 前缀操作**。不带前缀的键与 mihomo 配置语义完全一致；同一个文件里可以对同一个键做多种操作；一条规则推广到任意层级。没有采用 clashctl 的嵌套写法（`rules` 是列表就替换、是映射就执行操作，语义由类型决定，且只对写死的几个键有效），也没有用 YAML 标签（同一个键在一个文件里只能出现一次，无法既前插又追加）。

    | 写法 | 语义 |
    |---|---|
    | `键: 值` | 映射逐层合并，标量和列表整体替换。只写 `dns.nameserver` 不会清掉 `dns` 下的其他字段 |
    | `键: null` | 删除该键（同 RFC 7386 JSON Merge Patch） |
    | `prepend-键` / `append-键` | 在列表前插入 / 后追加，列表不存在时创建。顶层 `rules` 和 `sub-rules` 中的规则列表追加在末尾的 `MATCH` 之前，否则追加的规则永远不会命中 |
    | `patch-键` | 每一项按 `name` 找到列表中的项，用同样的语义递归修改它（可以在项里用 `prepend-proxies` 等）。找不到时警告，不报错：机场改名很常见 |
    | `delete-键` | 列表按 `name`（字符串列表按值）删除，映射按键删除。顶层删除节点或策略组时，同时从所有策略组的 `proxies` 中移除 |

  - 前缀可用在任何一层映射里，例如 `dns` 下的 `prepend-nameserver`、`sub-rules` 下的 `append-<名称>`。`patch-`、`delete-` 中的名称写成 `/正则/` 时匹配所有符合的项（用于过滤“剩余流量”“官网”这类信息节点、批量修改同一地区的节点）。
  - 同一层中按 替换 → 删除 → 修改 → 插入 的固定顺序执行，与书写顺序无关。这样先删掉订阅的 `MATCH` 再追加自己的 `MATCH`、删除信息节点不会误删自己追加的节点。
  - 顶层以 `x-` 开头的键不写入配置，用来放 YAML 锚点；锚点不能跨文件。mixin 中的注释不带入生成的配置。
- **多个 mixin 文件**：`/etc/tunlr/mixin.d/*.yaml` 按文件名排序，**逐个应用**，每个文件作用在前一个文件处理后的结果上。由此后面的文件优先级一致地更高：同一个键以后面的为准（plan 列出“哪个文件覆盖了哪个文件”），后面文件前插的规则排得更靠前，后面的文件可以 patch 前面文件加入的组。
  - 分类由文件名约定，工具不强制，例如 `10-dns.yaml`、`20-nodes.yaml`、`50-rules.yaml`。只读取 `.yaml`，改名为 `.yaml.off` 即停用；以点开头的文件（编辑器临时文件）被忽略。
  - 排序按文件名逐字符比较，与 `ls`、`run-parts`、systemd 的 drop-in 目录一致，不做“自然排序”（按数值比较数字前缀）：规则简单，用户看到的 `ls` 顺序就是应用顺序。代价是位数不同的前缀会出乎意料（`9-a.yaml` 排在 `10-b.yaml` 之后），因此 `plan` 发现数字前缀的数值顺序与应用顺序不一致时给出警告，并建议补零后的文件名；`tunlr edit` 新建文件时列出已有文件的应用顺序，方便选择前缀。
  - 子目录被忽略，预留给将来按订阅区分的 mixin（如 `mixin.d/<订阅名>/`，先应用全局文件，再应用启用订阅的文件）。第一版不做：patch、delete 找不到目标时只警告，切换订阅一般不会因此失败。
  - mixin 可能含有节点密码，目录 0700，文件 0600。
- **范围与检查**：
  - 强制项：mixin 写了会被忽略并给出警告，不报错，避免工具升级新增强制项后定时更新突然失败。
  - 允许但警告：数字前缀位数不同导致的意外顺序；`secret` 为空；`external-controller` 不在回环地址；`allow-lan` 开启且没有 `authentication`；用不带前缀的 `rules`/`proxies`/`proxy-groups` 整体替换订阅的内容；`patch-`、`delete-` 没有匹配的项。
  - 报错（退出码 3，不发布）：YAML 解析失败；前缀操作的值不是列表，或目标的类型不对（如 `prepend-dns`）；`patch-` 的项没有 `name`；正则无效；以及 `mihomo -t` 不通过。
  - mixin 有错时绝不丢掉它去发布（ShellCrash 的做法）：丢掉用户写的直连或拦截规则可能让流量走错路。定时更新遇到 mixin 错误同样保持当前配置。
- **`tunlr edit [名称]`**：同 visudo、`kubectl edit`，在临时副本上编辑，保存后合成并运行 `-t`，有错就显示原因并让用户选择重新编辑或放弃，**通过后才写回 `mixin.d`**，然后显示 diff、确认后发布。有错的编辑因此不会进入 `mixin.d`，定时任务读不到。
  - 新文件以带完整说明、全部注释掉的示例开始（`templates/mixin.yaml`），未修改就不保存。省略名称时从已有文件中选择或新建。
  - `-t` 因下载数据失败而未能完成时，可以选择只保存不发布。编辑期间文件被别人改过时不覆盖，编辑结果留在临时文件中。
  - 编辑器取 `$VISUAL`、`$EDITOR`，否则依次尝试 `vim`、`nano`、`vi`（有 vim 就用 vim，不经 Debian 的 `editor` 替代项，它常指向 nano）；需要终端，非交互模式下报用法错误，直接改文件后用 `plan`/`apply` 即可。
  - 打开编辑器前只说明 tunlr 自己的规则（保存退出后校验并显示差异、确认后才发布；不保存退出则不做改动），不讲编辑器本身的用法。
- 合成前展开订阅中的 YAML 锚点与别名（被引用的锚点可能位于被丢弃的键里），并限制展开后的节点数，防止恶意订阅放大。
- **校验前必须备好 geo 数据**。`mihomo -t` 会加载规则用到的 GeoIP/GeoSite 数据，缺失时现场从 GitHub 下载；在国内服务器上实测下载耗时 84 秒，超过 90 秒即判定校验失败，与配置本身无关。因此：
  - 工具从配置判断需要哪些 geo 数据（规则、DNS 设置中出现 geoip、geosite、ip-asn 即视为需要；宁可多备一个文件，不能漏），缺失的下载到内核工作目录（下载方式见第 7 节的镜像与校验）。已有的文件不在这里更新，由定时任务 `_update` 负责（见第 7 节）。预先备好后实测校验耗时 49 毫秒。
  - `-t` 在临时工作目录中运行，内核工作目录中的 geo 文件以符号链接提供：`-t` 可能写入缓存，直接用内核的工作目录会与运行中的内核冲突。
  - 校验失败时区分“配置错误”和“依赖下载失败”：mihomo 的错误消息含 `can't download` 时属于后者，不报告为配置问题（不映射到退出码 3）。
  - 实测 `-t` 不会抓取远程的 proxy-providers 和 rule-providers，它们暂时不可达不影响校验。
  - 内核自己的 geo 定时更新（`geox-url`）同样受网络影响，其地址由工具按镜像设置写入“工具生成的调整”。用户在 mixin 中自己指定的地址优先，因此不放在强制项中。
- **内核自己的下载**：

  | 下载 | 发起者 | 路径 | 校验 |
  |---|---|---|---|
  | 内核、校验前补齐的 geo 数据、面板（首次） | tunlr | 直连或经镜像 | 官方 sha256 |
  | 订阅 | tunlr | 直连 | 无（机场内容无从校验） |
  | 面板手动更新、面板上手动更新 geo | 内核 | 按规则分流，地址经镜像 | 无 |
  | geo 定时更新 | `_update` 触发内核下载 | 按规则分流，地址经镜像 | 事后由工具按官方 sha256 核对 |
  | 订阅中的 proxy-providers、rule-providers | 内核 | 按规则分流，或 provider 的 `proxy` 字段 | 无 |
  | 面板触发的内核升级 | 内核 | 按规则分流，直连 github.com | 无，事后由工具核对（见第 7 节） |

  - 读 mihomo 源码（`component/http`）确认：内核自己的 HTTP 请求交给内部 tunnel，按目标域名匹配规则，失败时才退回直连。所以策略组选中不可用的节点时，面板和 geo 下载全部失败（实测）。
  - **镜像直连规则**：配置了 `github-mirror` 时，工具在订阅规则前插入 `DOMAIN,<镜像域名>,DIRECT`（镜像是 IP 时用 `IP-CIDR`/`IP-CIDR6`）。镜像本来就是为直连准备的；实测选中不可用的节点时，加上这条规则后面板经镜像下载成功。它属于“工具生成的调整”而不是强制项：用户有理由让镜像也走代理，可以用 `delete-rules` 去掉。没有配置镜像时不加，直连 github.com 在国内很慢，交给规则决定。将来 TUN 阶段需要直连的主机（如订阅域名）也放在这一层。
  - **信任边界**：镜像可以篡改它转发的内容。工具的下载有官方 sha256 兜底，内核的下载没有。其中面板在浏览器中运行并持有 API secret，geo 数据决定分流，被篡改的后果都很严重，因此改由工具下载并校验：
    - 面板：apply 时若面板目录不存在，工具从 zashboard 最新发布下载 `dist.zip`，按官方 sha256 校验，解压（拒绝指向目录外的路径和非普通文件，限制解压后大小）后原子地放到位。实测内核启动时发现面板目录已存在就跳过下载。只处理 `external-ui-url` 仍是 zashboard 官方地址或其镜像地址的配置，用户在 mixin 中换了面板时交给内核。面板不是必需的：下载失败（时限 5 分钟）照常发布，由内核按 `external-ui-url` 自己下载；下载在加锁之前进行，不阻塞定时任务。
    - geo 数据：校验前缺失的文件由工具校验下载；定时更新由 `_update` 触发内核下载后按官方 sha256 核对（见第 7 节“定时更新”）。模板关闭内核自己的 `geo-auto-update`，它下载后不做任何校验。
    - `external-ui-url`、`geox-url` 仍写成经镜像的地址，用于 `_update` 触发的更新和用户在面板上主动点“更新”。
  - 实测：zashboard 与 meta-rules-dat 的发布资产都带有 GitHub 提供的 sha256 digest。
- **充分利用内核已有能力**，由工具写入模板默认开启：
  - `external-ui` / `external-ui-url` / `external-ui-name`：面板的位置和更新地址（首次由工具校验安装，之后可在面板上手动更新）
  - proxy-providers / rule-providers 的 `interval`：订阅与规则集热更新
  - `profile.store-selected` / `store-fake-ip`：保存面板选择的节点和 fake-ip 映射
  - `ntp`：时间同步（或交给系统的 timesyncd）
- 工具经 `external-controller-unix` 访问内核 API，路径以强制项写为相对路径 `api.sock`。内核相对于工作目录解析它，所以在临时目录中运行的 `-t` 不受影响。实测内核以 0666 创建这个 socket，访问也不需要 secret，因此只能靠工作目录的权限（0750）阻止其他本地用户。每次发布都会把工作目录的权限收紧到 0750。面板走 `external-controller`（HTTP，只监听本机），由 secret 保护：实测不带 secret 时返回 401。
- **发布分两步：预览（plan）与发布（apply）**（`pkg/core/pipeline`）：
  - plan：合成配置，与生效的配置比较，生成统一格式的 diff，并用 `mihomo -t` 校验。plan 不改动状态，只可能补齐校验需要的 geo 数据。内核未安装时 plan 仍可预览，但标明未校验；apply 则拒绝发布。
  - apply 先输出 plan 的结果，经确认后在文件锁内发布：原子写入 → 通知内核加载 → 记录历史。
    - 确认等待期间不持有锁，避免阻塞 timer 触发的任务。代价是加锁后要核对生效的配置与预览时一致，否则拒绝发布并要求重新预览，不覆盖用户没看过的变化。
    - 非交互模式下必须带 `--yes`，否则在开始工作前报用法错误。`--dry-run` 只预览。
  - 退出码：校验未通过为 3。内核加载失败时恢复原配置、让内核重新加载并返回 4，失败的配置不进历史。
  - 生效的配置被手工改过（与最新修订不一致）时，发布前先把它存为一个来源为 found 的修订，以便找回。
  - 生效的配置不存在（如卸载后重新 `init`）而发布的内容与最新修订相同时，沿用该修订，不重复记录。
  - **节点变化摘要**：diff 之前先给一行节点变化的摘要，如 `Proxies: 26 changed (port), 1 renamed (A → B)`，`--json` 中为 `proxies` 字段。节点按名称对应；一减一增的节点除名称外完全相同时视为改名，不按位置等线索猜测，名称和其他字段同时变化的（如实测端口也随机的信息节点）照常列为一增一删；只列出变化的字段名，不显示字段的值（密码等）。原因是实测机场每次抓取都会给一部分节点换随机端口，信息节点的名称带着剩余流量，每天的 diff 都有几十行，只看 diff 难以判断“只是端口变了”还是“机场换了节点”。摘要只是帮助阅读，不过滤任何变化：端口变了内核就必须重新加载。
- **热重载**（`pkg/core/mihomo.Client`、`Reloader`）：写入配置后经 API socket 调用 `PUT /configs?force=true`，`path` 填配置文件的绝对路径，然后用 `GET /version` 确认内核仍在响应。以下均为实测或读源码确认：
  - `path` 必须是绝对路径，且位于内核工作目录内，否则返回 400。
  - 必须带 `force=true`，否则端口等监听设置的变化不会生效；带上后端口变化即时生效。
  - 内核解析失败时返回 400 和错误信息，并继续运行原来的配置。
  - 内核应用配置的函数不返回错误，监听端口失败等问题不会反映在响应中，所以 204 只表示内核接受了这份配置。
  - 内核在重载时同步初始化 proxy-providers 和 rule-providers，新的 http provider 会现场抓取：地址不可达时实测重载耗时 3.4 秒。重载时限因此设为 2 分钟，超时按失败处理。
  - 内核退出后 socket 文件仍会留下，连接时被拒绝。socket 不存在和连接被拒绝都视为内核没有运行：配置照常发布，由内核启动时加载。
  - 重载失败（返回错误，或重载后不再响应）时，恢复原配置并让内核重新加载它，退出码 4。重载后内核不再响应时，即使随后连接被拒绝，也不能当作“没有运行”，同样回滚，让内核下次启动时用回原配置。
  - 热重载只调用 `executor.ApplyConfig`，不重建 API 服务；geo 定时器也只在启动时注册（读源码 `hub/hub.go`、`main.go`，并实测改 `secret`、`external-controller` 后重载不生效）。以下键变化时，apply 改为重启内核，plan 中说明“本次会重启内核”：`external-controller`、`external-controller-tls`、`external-controller-unix`、`external-controller-pipe`、`external-controller-cors`、`external-doh-server`、`secret`、`external-ui`、`external-ui-name`、`tls`、`geo-auto-update`、`geo-update-interval`。重启后做健康检查，失败时恢复原配置并再次重启，退出码 4。没有安装服务时照常写入，并提示需要重启内核。
- **回滚**：`rollback` 不带修订号时，回到最近一个与生效配置不同的修订，所以连续执行两次会在两个版本间来回切换（与 `helm rollback` 相同）。`rollback <号>` 恢复指定修订，`--list` 列出历史。回滚同样经过 `-t` 校验和确认，并记为新修订（来源为 rollback）。修订号递增、不复用。回滚恢复修订的内容后，强制项按当前状态重新写入（如透明代理开关，见 8.3）。回滚只恢复配置文件，下一次 apply（包括订阅更新）会按当前的订阅和 mixin 重新合成。

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
- **工具升级内核**（`tunlr upgrade core [--version <v>]`）：
  1. 下载并按官方 sha256 校验，解压到 `kernels/<版本>/`（此时尚未记为已知可用）。
  2. 降权用新内核对生效的配置执行 `-t`，不通过则停止（退出码 3），运行中的内核不受影响。
  3. 加锁，读入当前二进制备用（失败时换回）。
  4. 在 `bin/` 中写临时文件后 rename 替换，归 `tunlr` 所有。
  5. 服务在运行时重启并做健康检查；失败则换回原二进制、再次重启，退出码 4。服务没有运行时只替换，下次 `start` 通过健康检查后才记为已知可用。
  6. 记为已知可用，清理多余的版本（`kernels/` 中不在记录里的目录，包括校验未通过、下载后没用上的）。
  - 只有 `kernels/` 中工具下载校验过、sha256 与 `bin/mihomo` 一致的版本，才会在健康检查通过后记为已知可用。
  - `--rollback` 换到上一个已知可用版本，同样经过 `-t`、重启和健康检查。
- **面板升级的善后**（`_update`）：`bin/mihomo` 的 sha256 不在已知可用记录中时，查询运行中的版本（`/version`），下载该版本对应的官方资产解压后比较：一致则记为已知可用；不一致则换成校验过的二进制（下载不到时，如 alpha 版本，换回最近的已知可用版本）并重启。内核没有运行时跳过，交给救援。
  - 实测（tunlr-dev）：面板经 `POST /upgrade` 升级时，内核经节点下载 github.com 失败（`io.Copy(): unexpected EOF`），原二进制不受影响；以 `tunlr` 身份换上官方 v1.19.33 后 `_update` 核对通过并记为已知可用；换上末尾多一个字节的官方二进制（仍能运行）后被换成校验过的版本。
- **救援**：
  - `tunlr-core.service` 配置 `Restart=on-failure`、`RestartSec=3`、`StartLimitIntervalSec=120`、`StartLimitBurst=5`，以及 `OnFailure=tunlr-rescue.service`（执行 `tunlr _rescue`）。达到重启上限后服务进入 failed，触发救援。
  - `_rescue`：透明代理的规则在内核退出时已由 `ExecStopPost` 清理（见 8.4），直连和 SSH 不受影响。当前二进制不是已知可用版本时，换回最近的已知可用版本，`reset-failed` 后启动并做健康检查。已经是已知可用版本仍然起不来时，多半是配置或环境问题：不循环重启，保持停止，在日志中说明原因。
  - `upgrade core` 进行中持有锁并自己处理回滚；`_rescue` 拿不到锁时直接退出，不与它争抢。
  - 代价：面板升级出问题时，会有一个从启动失败到自动回滚的短暂断网窗口；工具升级则没有。README 中说明这一取舍。
  - 实测（tunlr-dev）：把 `bin/mihomo` 换成立即退出的脚本后，systemd 重启 5 次（约 20 秒）进入 failed，触发 `tunlr-rescue.service`，换回已知可用版本并通过健康检查。
- **unit 的其他设置**：`ExecStart=/var/lib/tunlr/bin/mihomo -d /var/lib/tunlr/core`；`CapabilityBoundingSet` 与 `AmbientCapabilities` 相同；`NoNewPrivileges=yes`、`ProtectSystem=strict`、`ReadWritePaths=` 只含 `bin/` 与 `core/`、`ProtectHome=yes`、`PrivateTmp=yes`；`LimitNOFILE=1048576`。实测这些设置下 TUN 可以正常工作，不需要额外的设备或 capability（见 8.2）；MVP 3 增加 `ExecStartPre`、`ExecStopPost`（见 8.4）。实测 Debian 12（systemd 252）上 `AmbientCapabilities` 对非 root 用户生效。
- **定时更新**（`tunlr-update.timer` 触发 `tunlr-update.service`，执行 `tunlr _update`）：每天一次，`OnCalendar=daily`、`RandomizedDelaySec=1h`、`Persistent=true`。`init` 用 `enable --now` 启动 timer：只启用不启动的话要到下次开机才开始计时（实测）；第一次启动不会立即触发。依次：
  1. 更新启用的订阅，失败时保留上一次的内容。
  2. 按当前订阅和 mixin 合成并发布，与 `apply --yes` 相同；有变化才发布，检查不通过则保持当前配置。
  3. 面板升级的善后（见上）。
  4. geo 数据更新。
  - 任何一步失败都记录在日志中，继续后面的步骤，最后以非零退出码结束，让 systemd 记为失败。每一步的结果（ok、skipped、failed 和说明）输出到 journal。
- **geo 数据更新**（读源码 `component/updater/update_geo.go` 并实测）：
  - 内核的 geo 数据只能由内核自己的更新器重新加载（`mmdb.ReloadIP` 等只在更新器中调用）；工具替换文件后要重启内核才生效。
  - `POST /configs/geo` 同步执行：内容与磁盘上的相同时直接返回 204，不写文件也不记日志（之前“没有结果”的原因）；单个文件的下载时限 90 秒，经 gh-proxy 实测一次 64 秒成功、一次 90 秒超时。
  - 所以 `_update` 先调用 `POST /configs/geo` 让内核经镜像更新，无需重启；成功后由工具查询 meta-rules-dat 最新发布的官方 sha256，核对工作目录中内核正在使用的 geo 文件。不一致时下载校验过的文件（经镜像校验不通过时改为直连）替换并重启内核；拿不到校验过的文件则记录错误。内核下载失败（如超时）时本次跳过，留给下一次。
- **状态以实际为准**：版本、TUN 状态等一律通过 API 查询内核实际情况；工具自己的记录只作为“已知可用的历史”。
- 注意：内核 API 并不稳定（例如曾有标注为 break 的 `/proxies` 行为变更），API 调用集中封装在 `pkg/core/mihomo` 的 client 中，并随 mihomo 发版做兼容性测试。

## 8. 透明代理

> 8.3～8.6 是 MVP 3 的设计，根据 8.2 的实测结果写成，已经实现，实现后的实测见 8.7。MVP 3 只做本机模式，网关模式（`ip_forward`、转发策略等）留到 MVP 5。

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
- **配置复用分层合成**，`tun on/off` 本身只改开关，然后走与 `apply` 相同的流水线（`-t` 校验、原子写入、热重载、失败回滚、记入历史）。实测热重载即可开关 TUN，`tun` 不加入需要重启的键。
  - 模板（用户可以用 mixin 覆盖）：`stack: mixed`、`auto-detect-interface: true`、`dns-hijack: [any:53, tcp://any:53]`，附注释说明为什么不排除私有地址段。
  - 强制项（mixin 无法覆盖，写了就警告并忽略）：`tun.enable`（取自开关：`pending` 与 `on` 为 true）、`auto-route: true`、`auto-redirect: true`、`strict-route: false`、`device: tunlr0`、`iproute2-table-index: 2022`、`iproute2-rule-index: 9000`。工具补的规则、清理和检测都依赖这些值；固定网卡名也让 `doctor` 和日志一眼认出它。
  - 不写进 `mixin.d`，理由与“工具生成的调整”相同。
- **回滚与开关**：`rollback` 恢复旧修订的内容后，再按当前开关写入强制项（与合成时最后一层相同）。否则回滚到开关不同的修订，配置就与工具的规则和防失联状态对不上。`plan` 照常显示差异。
- **面板运行时切换**（见 3.3）：面板打开 TUN 不写回配置，也不经防失联检测。工具的规则不依赖开关（见 8.4），所以面板打开时同样生效。`status` 在内核实际状态与开关不一致时提示，执行 `tun on/off` 即以开关为准。

### 8.4 工具补充的规则及其生命周期

- 规则放在独立的 `inet tunlr` 表中，模板 `templates/nft/tunlr.nft`，经 `nft -f` 原子加载，整表删除即清理：
  - **入站连接的回包走主路由表**：从 `tunlr0` 以外进入、目标为本机（`fib daddr type local`，此时尚未 DNAT）的连接，在原方向（`ct direction original`）的包经过时打上 conntrack 标记。不限于新连接：加载本表之前已经建立的连接（如执行 `tun on` 的 SSH 会话）在对方发来下一个包时也会标记，所以 `tun on` 在发布前先加载一次本表；属于这些连接的回包（本机进程发出的走 `output` route 链，容器发出的走 `prerouting`）带上包标记，`ip rule fwmark … lookup main`（优先级 8999，早于内核的 9000）。这修好了 Docker 发布的端口，也让 SSH 等所有入站连接不依赖内核规则的细节。
  - 标记只用一个高位 `0x10000000`，按位设置（`meta mark set meta mark | 0x10000000`），不覆盖其他程序的标记：Tailscale 用 `0xff0000` 段，kube-proxy 用 `0x4000`、`0x8000`，内核的 auto-redirect 用 `0x2023`、`0x2024`。
  - ip rule 由 iproute2 的 `ip` 命令维护（IPv4、IPv6 各一条）。
- **与内核服务同生共死**，不依赖开关：规则在 TUN 关闭时无害（标记的回包本来就走主路由表），这样面板临时打开 TUN 也有效。
  - `tunlr-core.service` 增加 `ExecStartPre=-+/usr/local/bin/tunlr _prestart`：加载 `inet tunlr` 表和 ip rule（`+` 表示以 root 运行，不受 `User=` 和沙箱限制；`-` 表示失败也照常启动内核，不让规则的问题拖垮代理本身）。
  - 增加 `ExecStopPost=-+/usr/local/bin/tunlr _poststop`：删除 `inet tunlr` 表和工具的 ip rule；内核的 `inet mihomo` 表和优先级 9000～9010 的 ip rule 如果还在（内核被 `SIGKILL` 或崩溃），一并删除。`ExecStopPost` 在进程以任何方式退出后都会执行，所以实测中 `SIGKILL` 后容器断网的问题由此消除，`_rescue` 不必再单独清理。
  - 这两个命令不取文件锁（`tunlr start` 持锁调用 `systemctl start` 时会死锁），操作本身幂等。
- **规则被他人清空**：例如重新加载 Debian 的 `nftables.service`，其配置以 `flush ruleset` 开头，会连同内核的表一起清空。增加 `tunlr-check.timer`（每 5 分钟），执行 `_check`：内核在运行而 `inet tunlr` 不见了，重新加载；内核开着 TUN 而 `inet mihomo` 不见了，重启内核由它重建。另外兜底处理回滚定时器没有生效的情况：开关为 `pending` 且过期一分钟以上时关闭 TUN。

### 8.5 开启流程与防失联

`tun on`：

1. 前置检查，不通过则退出码 3：服务已安装且内核在运行（不运行就无法检测，提示先 `start`）；`/dev/net/tun`、`nft`、`ip` 可用；优先级 8999～9010 没有他人的 ip rule；启用了 UFW、firewalld 时给出警告（未实测，见 8.8）。
2. 显示 `plan`（diff 中是 `tun.enable` 等强制项的变化）并确认；非交互模式需要 `--yes`，`--dry-run` 只预览。
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

- `apply`、`_update`、`sub use` 照常合成，强制项随开关写入，定时更新不会改变 TUN 状态。
- `status`：开关（`pending` 时显示剩余时间）、内核实际的 TUN 状态、两张表是否存在。
- `uninstall`：停止服务时 `_poststop` 清理规则（服务没在运行或 unit 是旧版时再直接清理一次）；删除 `tunlr-check.timer`，停止可能存在的回滚定时器；开关记为 `off`，重新 `init` 后 TUN 保持关闭。
- `doctor`（MVP 4）复用第 1 步的前置检查。
- 代码：规则模板、加载与清理、前置检查在 `pkg/core/netfilter`；开关与开启流程在 `pkg/core/setup`；CLI 只有 `internal/cli/tun.go`。nft 模板做 golden 测试，规则本身按 CLAUDE.md 在 network namespace 中做集成测试。

### 8.7 实现后的实测

tunlr-dev（环境同 8.2；SSH 客户端 192.168.11.41 不在服务器的直连网段，回包按默认路由本会进 TUN）：

| 项目 | 结果 |
|---|---|
| nft 规则（network namespace 集成测试） | 通过：重复加载幂等；模拟内核残留后 `Down` 清理干净，他人的 ip rule 保留 |
| `init` 升级旧安装 | unit 带上 `_prestart`、`_poststop`，`tunlr-check.timer` 启用，规则随内核启动加载 |
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

### 8.9 若将来实现 tproxy，需要处理的 Docker 冲突

- 容器发布端口的回包会被劫持 → `ct direction reply return`
- tproxy 挂在 mangle（-150），早于 Docker 的 DNAT（-100） → `fib daddr type local return`
- 显式用 `iifname` 列出要拦截的接口（docker0、`br-*` 是否代理属于策略选择）
- 用 mihomo 的 `routing-mark` 防止回环；局域网与保留地址直接 return
- 规则放在独立的 `inet tunlr` 表中，清理时整表删除；`_check` 检测规则是否被他人 flush

## 9. 从 ShellCrash 借鉴的功能

纳入：

1. **本机模式与网关模式**：`init` 时选择，二者需要的 sysctl、防火墙、DNS 设置不同。（MVP）
2. **端口访问安全**：代理端口只监听 127.0.0.1，或在允许局域网访问时强制设置认证，避免成为开放代理。（MVP）
3. 流量过滤开关：按 IP/网段过滤局域网设备、只代理常用端口、屏蔽 QUIC、容器与虚拟机流量独立开关（翻译为 TUN 排除地址或 mihomo 规则）
4. 自定义保留地址段 → `route-exclude-address`
5. 开机联网检测与防自启死循环 → `network-online.target` + `StartLimitBurst`
6. 失败通知：通用 webhook，覆盖更新失败、自动回滚、订阅即将到期
7. 版本回退（已规划）
8. 诊断与查看当前防火墙规则（`doctor`、`plan --full`）
9. 备份与还原

由内核能力替代：保存面板节点、规则集定时更新、NTP、订阅热更新（见第 6 节）。

不做：见 3.4。

## 10. 命令设计

```
tunlr init                首次安装向导
tunlr status              当前状态（服务、版本、订阅、透明代理、漂移）
tunlr doctor              诊断

tunlr start | stop | restart | logs     服务控制

tunlr plan                预览变更（--full 输出将写入系统的全部文件）
tunlr apply               应用变更（默认先展示 diff，--yes 跳过确认）
tunlr rollback            回滚配置（--list 查看历史）
tunlr edit [名称]         用 $EDITOR 编辑 mixin.d 中的文件，校验通过才保存，然后发布

tunlr sub add | list | remove | update  订阅（可保存多个）
tunlr sub use [name]      切换启用的订阅（同一时间只启用一个；交互时省略名称则从列表选择）
tunlr tun on | off        透明代理（--mode 预留给将来的 tproxy）
tunlr env on | off        当前 shell 的代理变量（需先配置 shell-init）

tunlr upgrade             升级 tunlr 自身
tunlr upgrade core        升级内核（--rollback 回退）

tunlr panel               面板地址与 SSH 隧道命令
tunlr config              查看和修改工具设置（交互式，或 config set <key> <value>）
tunlr backup | restore    备份与还原
tunlr shell-init          输出 shell 函数
tunlr completion          shell 自动补全
tunlr uninstall           完整卸载并清理规则
tunlr version
```

内部命令（供 systemd 调用，不显示在帮助中）：`tunlr _check`、`tunlr _rescue`、`tunlr _update`，以及透明代理用的 `_prestart`、`_poststop`、`_tun-guard`（见 8.4、8.5）。

服务与安装相关命令的行为：

- `init`：可重复执行，再次执行即修复。依次：检查环境（root、systemd、架构）→ 把自身复制到 `/usr/local/bin/tunlr` → 创建用户 `tunlr` → 建目录并设置属主与权限 → 没有内核时安装最新版 → 写入 unit 和 timer、`daemon-reload`、启用 `tunlr-core.service` 与 `tunlr-update.timer` → 给了订阅就添加 → 有启用的订阅时发布配置并启动内核、做健康检查。交互模式下询问 GitHub 镜像和订阅地址（都可以留空）；非交互模式由 `--mirror`、`--sub <名称>=<地址>` 给出，没给就跳过。
- `start`、`stop`、`restart`：执行 `systemctl`，`start`、`restart` 之后做健康检查。`logs` 执行 `journalctl -u tunlr-core.service`，透传 `-f`、`-n`。
- `status`：服务状态、运行中的内核版本、已知可用版本、启用的订阅、最近一次发布、下次定时更新的时间。
- `uninstall`：停止并禁用、删除 unit，删除 `bin/`、`core/`、`kernels/` 和用户 `tunlr`；保留 `/etc/tunlr`、订阅、历史、状态文件和 `/usr/local/bin/tunlr`（之后可以直接 `tunlr init` 恢复），`--purge` 才一并删除。需要确认（`--yes`）。
- `sub use [名称]`：交互模式下询问是否立即发布，`--apply` 直接发布（同 `apply`，非交互时需配合 `--yes`）。

**从已有条目中选择**：命令的参数是从已有条目中挑一个（或几个）时，交互模式下可以省略参数，改为在列表中用上下箭头选择、回车确定（多选时 x 或空格勾选），Esc 或 Ctrl-C 取消，取消不算失败。不提供 `/` 过滤：列表都不长，而过滤模式要占用 Esc（huh 默认只认 Ctrl-C 为取消，工具把 Esc 也加上）。非交互模式下省略参数的行为不变：原来必须给的照样报用法错误，原来有默认值的（如 `rollback` 回到上一个不同的修订）照样用默认值，带 `--yes` 时也不询问。选项要能放进 80 列的终端，细节留给 `sub list`、`rollback --list`。列表中每项附上做选择需要的信息，光标停在最可能的那一项上：

| 命令 | 列表 | 光标起始 |
|---|---|---|
| `sub use` | 全部订阅，标出启用的那个，附节点数、更新时间、流量与到期日 | 启用的订阅 |
| `sub remove` | 启用的以外的订阅（多选） | 第一项 |
| `rollback` | 历史修订，附时间、来源、订阅，标出与生效配置相同的那一份 | 不带参数时会回到的那一份 |
| `edit` | mixin 文件，末尾是“新建文件” | 第一项 |

**主菜单**：在终端里只输入 `tunlr` 时进入主菜单；非交互模式（包括 `--json`）仍然显示帮助。菜单不是另一套界面，只是子命令的入口，全部用 huh 的字段实现，不做全屏 TUI：

- 两级。第一级是“查看状态”和四个分类，分类右侧显示当前状态，最后是“退出”：

  | 分类 | 右侧状态 | 第二级 |
  |---|---|---|
  | Subscriptions | 启用的订阅 · 共几个 | 列出、切换、更新、添加、删除 |
  | Configuration | 当前修订号 | 预览、预览并发布、编辑 mixin、回滚、列出修订 |
  | Transparent proxy | 开 / 关 / 检查中 | 开启或关闭、查看详情 |
  | mihomo service | 运行中 · 版本 / 未运行 | 启动或重启、停止，最近的日志，升级内核 |

  第二级末尾是“← Back”。没安装时第一级只有“安装（init）”“查看状态”“退出”。菜单项随状态变化：有订阅才有切换、更新；订阅多于一个才有删除；有历史才有回滚。
- 每个画面先清屏：先输出一屏空行把旧内容推进回滚缓冲区，再回到左上角，往上翻仍看得到之前的输出。列表标题是所在位置（`tunlr › Subscriptions`），光标停在上次在这一级选的那一项。
- 选中操作后进入这一操作的“页”：顶部是操作名和等价命令（订阅地址不含令牌），下面是命令的输出。结束后底部给出结果（`✓ Done`、`✗ Failed`、`✗ Failed; the change was rolled back`、`– Interrupted`），按回车或 Esc 返回菜单，结果不会一闪而过。操作开头的提问被取消（什么都没做）时直接回到菜单。
- 按键规则处处相同：任何提问（列表、输入、确认）按 Esc 或 Ctrl-C 取消，回到上一级；在第一级按 Esc 退出菜单。列表和结果页不需要输入文字，`q` 也能取消（与 less 一致）。
- 取消的提问必须像提交一样正常退出。huh 在用户取消时发送 `tea.Interrupt`，Bubble Tea（v2.1.0 仍是如此）把中断当作程序被杀死，不等读取输入的协程退出就返回；紧接着运行的下一个提问或菜单与这个残留的协程争抢输入，按键时而被它读走丢掉。在 tunlr-dev 上约一半的菜单流程因此丢键，曾把方向键丢掉而误执行了“停止内核”。所以每个表单都带一个 `tea.WithFilter`：表单已标记为取消时，把它发出的中断换成正常退出，`Run` 照样返回 `huh.ErrUserAborted`；不是用户取消的中断保持原样。不要用每次打开 `/dev/tty` 的办法绕开。操作执行中按 Ctrl-C 只中断这一个操作（照常走事务的回滚），然后进入结果页，不会退出菜单。
- 添加订阅时名称和地址在同一个表单里，可以用 Shift-Tab 回到名称修改，两项都当场校验。
- 不放 `uninstall`：卸载不该是在菜单里顺手按到的操作。
- 子命令失败不退出菜单：错误照常报告，结果页显示失败。

**输出样式**：标准输出是终端、且没有设置 `NO_COLOR` 或 `TERM=dumb` 时，用 lipgloss 加样式，只用 16 色：

- 耗时的步骤（合成与校验、发布、抓取订阅、启动与升级内核、`init` 与 `tun on` 的各步）前显示 spinner，完成后换成 ✓ 或 ✗ 加耗时。步骤进行中的日志经由同一个输出写在 spinner 上方，不会和它挤在一行。
- 下载内核、geo 数据时，spinner 的那一行带进度条、已下载/总大小和速度（用 bubbles `progress` 的 `ViewAs` 只渲染字符串，不起 Bubble Tea 程序）。进度经 context 传给 `pkg/core/download`（`download.WithProgress`），core 不依赖任何界面库。
- diff 中增加的行绿色、删除的行红色、段头青色；检查结果用 ✓ ✗ –；状态值按好坏上色。
- 长输出（`plan`、`apply` 的计划与 diff，`logs`）超过一屏时像 git 一样交给分页器：`$PAGER`，默认 `less`，未设置 `LESS` 时用 `FRX`（不足一屏直接输出、保留颜色、退出后内容留在屏幕上）。只在标准输入输出都是终端时分页。
- 确认发布时，问题下方给出节点变化的一行摘要，不必回头翻 diff。
- huh 的提问用 `ThemeBase16`，跟随终端的 16 色，与其余输出一致。
- 其他情况（管道、重定向、systemd 运行的内部命令、`--json`）输出与不加样式时逐字相同：步骤为 `==> 步骤`，结果为 `ok`、`failed`、`skipped` 文字列，脚本照常解析。

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
- 可审计性：`tunlr plan --full` 输出将写入系统的全部内容（mihomo 配置、nft 脚本、systemd unit），用户执行前即可审查。
- 名字的读音和来历写在 README 与官网首页；提供 shell 自动补全，`shell-init` 可选提供短别名。

## 13. MVP 顺序

1. 配置流水线：模板 + mixin → 校验 → 原子写入 → 重载 → 失败回滚
2. systemd 托管与内核安装、升级、已知可用版本
3. 透明代理开关（TUN + auto-redirect），带自动检测与超时回滚
4. `doctor` 诊断
5. 本机/网关模式、端口安全（ShellCrash 借鉴项 1、2）

其余功能在此之后按需求排期。

## 14. 待决事项

- 开源协议
- 商标查询
