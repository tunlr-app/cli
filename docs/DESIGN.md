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
- **内核升级**：面板可以通过内核自带接口升级；工具负责兜底——保留已知可用版本，内核启动失败时自动回滚（见第 7 节）。工具自己升级时会先做兼容性校验，没有断网窗口。
- **重载配置**：面板重载读的是工具生成的文件，不冲突。
- **geo 更新**：定时更新写在配置里由内核完成，面板手动刷新作为补充。
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
| `/etc/tunlr/` | mixin（`mixin.yaml`）、工具设置（`settings.yaml`） | 用户可编辑，备份时最需要保留 |
| `/var/lib/tunlr/state.json` | 状态文件：结构版本号、安装实例 ID | 工具维护，权限 0600 |
| `/var/lib/tunlr/subscriptions/` | 每个订阅最近一次成功抓取的内容 | 含机场凭据 |
| `/var/lib/tunlr/history/` | 已发布配置的历史：每个修订一份配置（`<号>.yaml`）和一份记录（`<号>.json`） | 用于回滚，保留最近 10 个，权限 0600 |
| `/var/lib/tunlr/kernels/<版本>/` | 已知可用的内核 | 见第 7 节 |
| `/var/lib/tunlr/bin/mihomo` | 运行中的内核，也用于校验配置 | 归运行内核的专用用户所有，面板升级时原地替换 |
| `/var/lib/tunlr/core/` | mihomo 工作目录（`-d`）：生效的配置、API socket（`api.sock`）、geo 数据、面板、缓存 | 归运行内核的专用用户所有，权限 0750（保护 API socket） |
| `/run/lock/tunlr.lock` | 文件锁 | 放在 `/run` 下，安装前也可用 |

- 所有路径由 `pkg/core/paths.Layout` 统一给出，测试时设置 `Root` 把整套布局放进临时目录。
- 文件一律经 `pkg/core/fsutil.WriteFile` 原子写入：同目录临时文件 → 刷盘 → `os.Rename` → 刷新目录。
- 锁被占用时重试到期限为止，超时报告“另一个 tunlr 操作正在进行”。锁文件不删除，否则并发的获取者会锁住不同的文件。
- 状态文件读取时检查版本：比当前工具新则拒绝并提示升级，比当前旧则按迁移链逐级升级。
- 工具设置（`pkg/core/settings`）由用户编辑，文件不存在时用默认值；未知的键视为错误，避免拼写错误被静默忽略。目前的设置项：`github-mirror`（GitHub 下载镜像前缀）、`user-agent`（抓取订阅的默认 User-Agent）。

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
  - mixin 对所有订阅共用。它可能引用某个机场特有的策略组，切换后若 `mihomo -t` 不通过，按失败回滚处理并提示原因。
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
  1. 模板：工具内置的系统部分，如端口、DNS、geo 更新、面板、profile。
  2. 默认值：工具为这台服务器生成的值，mixin 可以覆盖。包括 HTTP API 的 `secret`（第一次发布时随机生成，存在状态文件中），以及配置了 `github-mirror` 时经镜像的面板地址（`external-ui-url`）和 geo 数据地址（`geox-url`）。
  3. 订阅：**只取路由部分**，即 `proxies`、`proxy-groups`、`rules`、`proxy-providers`、`rule-providers`、`sub-rules`，其余键一律丢弃并在 `plan` 中列出。订阅是不可信输入，不能让机场配置打开端口、改监听地址或设置 API。
  4. mixin：用户的覆盖，可以改任何键。
  5. 工具强制项：工具运行所依赖的键（目前是 `external-controller-unix`，将来还有 TUN 开关），最后写入，mixin 无法覆盖。
- **mixin 只支持少数明确的合并语义**，保证结果可预测：
  - 普通键：映射逐层合并，标量和列表整体替换。例如只写 `dns.nameserver` 不会清掉 `dns` 下的其他字段。
  - `prepend-<键>` / `append-<键>`：在顶层列表前插入或后追加，如 `prepend-rules` 插入自定义规则。命名沿用 Clash Verge 的 Merge 语法，降低迁移成本。
  - 不做按名称匹配列表元素等智能合并。
- 合成前展开订阅中的 YAML 锚点与别名（被引用的锚点可能位于被丢弃的键里），并限制展开后的节点数，防止恶意订阅放大。
- **校验前必须备好 geo 数据**。`mihomo -t` 会加载规则用到的 GeoIP/GeoSite 数据，缺失时现场从 GitHub 下载；在国内服务器上实测下载耗时 84 秒，超过 90 秒即判定校验失败，与配置本身无关。因此：
  - 工具从配置判断需要哪些 geo 数据（规则、DNS 设置中出现 geoip、geosite、ip-asn 即视为需要；宁可多备一个文件，不能漏），缺失的下载到内核工作目录（下载方式见第 7 节的镜像与校验）。已有的文件不更新，由内核的 geo-auto-update 负责。预先备好后实测校验耗时 49 毫秒。
  - `-t` 在临时工作目录中运行，内核工作目录中的 geo 文件以符号链接提供：`-t` 可能写入缓存，直接用内核的工作目录会与运行中的内核冲突。
  - 校验失败时区分“配置错误”和“依赖下载失败”：mihomo 的错误消息含 `can't download` 时属于后者，不报告为配置问题（不映射到退出码 3）。
  - 实测 `-t` 不会抓取远程的 proxy-providers 和 rule-providers，它们暂时不可达不影响校验。
  - 内核自己的 geo 定时更新（`geox-url`）同样受网络影响，其地址由工具按镜像设置写入默认值层。用户在 mixin 中自己指定的地址优先，因此不放在强制项中。
- **充分利用内核已有能力**，由工具写入模板默认开启：
  - `geo-auto-update` / `geo-update-interval`：geo 数据定时更新
  - `external-ui` / `external-ui-url` / `external-ui-name`：内核自行下载面板
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
- **回滚**：`rollback` 不带修订号时，回到最近一个与生效配置不同的修订，所以连续执行两次会在两个版本间来回切换（与 `helm rollback` 相同）。`rollback <号>` 恢复指定修订，`--list` 列出历史。回滚同样经过 `-t` 校验和确认，并记为新修订（来源为 rollback）。修订号递增、不复用。回滚只恢复配置文件，下一次 apply（包括订阅更新）会按当前的订阅和 mixin 重新合成。

## 7. 内核管理与救援

- **工具升级内核**：下载到临时目录并校验 checksum → 用新二进制对当前配置执行 `-t` → 通过后替换并保留旧版本 → systemd 重启 → 健康检查失败则换回旧版本。
- **按 CPU 指令集级别选择内核构建**：mihomo 的标准 amd64 构建要求 x86-64-v3（AVX2、BMI2、FMA 等），在更低级别的 CPU 上直接报错退出；另有 `compatible` 构建面向低级别 CPU。虚拟机很常见这种情况：实测一台 KVM 虚拟机的 CPU 类型配置为通用型号时只暴露 x86-64-v1，标准构建无法运行。
  - 安装和升级时读取 `/proc/cpuinfo` 的 flags 判断级别，v3 及以上用标准构建，否则用 `compatible`。面板触发的内核升级由内核自己选择构建，救援流程需要能识别并回滚到可运行的版本。
  - `doctor` 报告 CPU 级别；低于 v3 时提示虚拟机可改用 host 或更新的 CPU 型号以获得更好的性能（同时会带来 AES-NI）。
- **镜像下载，官方校验**：国内服务器直连 GitHub 下载很慢（实测 2 分钟只下完 22MB 中的 6.5MB），经 gh-proxy 等镜像只需约 12 秒。工具支持配置 GitHub 下载镜像（内核、geo 数据、面板，以及安装脚本），但**校验值必须取自官方来源**：从 `api.github.com` 读取 release 资产的 `digest`（sha256），镜像只负责传输字节，校验不一致则丢弃；GitHub 没有提供 digest 的资产拒绝下载。api.github.com 在国内通常可以直连。实现见 `pkg/core/download`。
- **兼容面板升级**：面板通过内核接口升级时会原地替换二进制，因此：
  - 工具在每个版本被确认运行正常后，把它复制到 `/var/lib/tunlr/kernels/<版本>/` 作为已知可用版本。
  - systemd unit 配置 `Restart=on-failure`、`StartLimitBurst`，以及 `OnFailure=` 触发 `tunlr _rescue`。
  - `_rescue` 先清理 TUN/auto-redirect 残留的 nftables 规则以恢复直连和 SSH，再在二进制与已知可用版本不一致时自动回滚并重启。
  - 代价：面板升级出问题时，会有一个从启动失败到自动回滚的短暂断网窗口；工具升级则没有。README 中说明这一取舍。
- **权限**：mihomo 以专用的非 root 用户运行，通过 systemd `AmbientCapabilities` 只授予 `CAP_NET_ADMIN` 和 `CAP_NET_BIND_SERVICE`。为了让面板升级可用，内核二进制放在该用户拥有的目录中；external-ui 目录同样可写。
- **状态以实际为准**：版本、TUN 状态等一律通过 API 查询内核实际情况；工具自己的记录只作为“已知可用的历史”。
- 注意：内核 API 并不稳定（例如曾有标注为 break 的 `/proxies` 行为变更），API 调用集中封装在 `pkg/core/mihomo` 的 client 中，并随 mihomo 发版做兼容性测试。
- 内核自升级的具体实现（是否校验 checksum、重启方式）需查阅 mihomo 源码中的 updater 确认，这会影响 unit 的写法。

## 8. 透明代理

### 8.1 默认方案：TUN + auto-redirect

- 配置：开启 `auto-route` 与 `auto-redirect`，**关闭 `strict-route`**（它会让本机无法被其他设备访问），用 `route-exclude-address` 排除局域网和保留地址段。
- 理由：auto-redirect 用 nftables 处理 TCP，避开了 TUN 回包被 rp_filter 丢弃的问题；sing-box 文档称其路由更好、性能优于 tproxy，并能避免与 Docker 网桥冲突。工具不需要自己写一整套 nftables 规则。
- 已知局限：auto-redirect 只接管 TCP，UDP 仍走 TUN 路由。服务器上的入站 UDP 服务（WireGuard、hysteria2 服务端、DNS 等）可能受影响。
- 工具负责的前置检查：`net.ipv4.ip_forward`（网关模式）、防火墙 forward 策略（如 UFW 默认 DROP）、53 端口与 systemd-resolved 的冲突。

### 8.2 上线前需要实测

1. 从外部 SSH 到服务器
2. 访问服务器上的 TCP 服务
3. 访问服务器上的 UDP 服务
4. 服务器上 Docker 容器的联网

全部通过则维持默认方案；只有入站 UDP 等场景确实不满足时，才实现自写 tproxy（`tun on --mode tproxy`）。

### 8.3 若将来实现 tproxy，需要处理的 Docker 冲突

- 容器发布端口的回包会被劫持 → `ct direction reply return`
- tproxy 挂在 mangle（-150），早于 Docker 的 DNAT（-100） → `fib daddr type local return`
- 显式用 `iifname` 列出要拦截的接口（docker0、`br-*` 是否代理属于策略选择）
- 用 mihomo 的 `routing-mark` 防止回环；局域网与保留地址直接 return
- 规则放在独立的 `inet tunlr` 表中，清理时整表删除；`_check` 检测规则是否被他人 flush

### 8.4 防失联

开启透明代理时先创建临时回滚定时器，再自动检测网络（访问若干测试地址、确认 SSH 未中断）。检测通过则自动取消回滚；检测失败或会话中断，则在超时后自动关闭透明代理并清理规则。用户不需要手动确认。

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
tunlr edit                用 $EDITOR 编辑 mixin，保存后自动校验

tunlr sub add | list | remove | update  订阅（可保存多个）
tunlr sub use <name>      切换启用的订阅（同一时间只启用一个）
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

内部命令（供 systemd 调用，不显示在帮助中）：`tunlr _check`、`tunlr _rescue`、`tunlr _update`。

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

- 透明代理是否需要 tproxy 实现（取决于第 8.2 节实测）
- 开源协议
- 商标查询
