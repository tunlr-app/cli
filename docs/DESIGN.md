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
- 曾有同名的 Smart DNS 服务（已停运），正式推广前查询商标。

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
- **geo 更新**：由内核按配置自己下载和更新（面板手动刷新同样可以），工具不下载也不校验；兜底的地址和周期是 tunlr 内置的受管 mixin `tunlr:defaults`，订阅写了的以订阅为准，按订阅用 `sub set <订阅> --geo-update、--geo-url` 调整（见第 7 节）。
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

- **定位**：`status` 回答“现在是什么状态”，要快；`doctor` 回答“为什么不正常、这台机器能不能用”，可以慢一些、更彻底。doctor 只读、不修复，每个问题给一条修复建议（多数是 `tunlr init`、`tunlr core start` 等已有命令：`init` 本来就是修复）。定时任务不调用它。
- **四组检查**：

  | 组 | 检查 | 何时 |
  |---|---|---|
  | `system` 支持条件 | Linux、root、PID 1 为 systemd、amd64 或 arm64（amd64 报告 CPU 级别，低于 v3 警告）、不在 WSL 中、不在容器中、发行版版本 | 总是。`init` 复用：有 fail 就拒绝安装 |
  | `tun` 透明代理条件 | `/dev/net/tun`、`nft`、`ip`、需要的 ip rule 优先级没有被占用、默认丢弃入站又不放行 DNAT 连接的规则链（会丢掉内核重定向的 TCP，见 8.10“其他防火墙”）、iptables-legacy 中的规则 | 总是。`tun on` 复用：有 fail 就不开启，warn 照常显示 |
  | `install` 安装是否健康 | unit 与当前版本的模板一致；`/usr/local/bin/tunlr` 与正在运行的这个相同；内核用户存在，`bin/`、`core/` 归它所有；内核服务已启用、在运行，没有 failed 或反复重启；内核二进制存在且是已知可用版本；设置和状态文件能读取；生效配置存在并通过 `mihomo -t`；两个 timer 与设置和 TUN 开关一致，没有遗留的回滚定时器；TUN 开关与内核实际状态一致，内核在运行时两张 nft 表都在；内核开着 TUN 时防回环的两道设置都生效（见 8.4）；配置中的端口没有被其他程序占用；订阅最近一次抓取是否失败、是否已到期或 7 天内到期、流量是否用完或超过 90% | 已安装（有 `tunlr-core.service`）时。没有安装时只有一项 skip，提示 `tunlr init` |
  | `network` 网络 | 能解析域名；能访问 api.github.com（校验值的来源）；配置了镜像时镜像能连上；本机时钟与 GitHub 的偏差（超过 90 秒警告，VMess 等协议会失败；超过 1 天判失败，TLS 会失败）；内核在运行时经 mixed-port 访问一个测试地址 | 默认执行，并行进行，总时长约 10 秒；`--offline` 跳过 |

- **分级**：ok、warn、fail、skip。有任何 fail 时退出码 3；只有 warn 时为 0。
  - WSL、容器判 fail（第 3.4 节明确不做），`init` 直接拒绝，不提供跳过检查的参数：不做兜底兼容。
  - 发行版不在 Debian 12+、Ubuntu 22.04+ 之列时判 warn，只要是 systemd 系统就允许安装。
  - iptables-legacy 中有规则时判 warn：只影响透明代理，不拒绝安装。
  - 订阅：启用的订阅已到期或流量用完判 fail，其余订阅判 warn。
- **非 root 运行**：照常运行，root 一项 fail，需要 root 才能查的项为 skip，用户仍能看到完整的报告。
- **输出**：按组输出，每项一行；warn、fail 下一行给出修复建议。`--json` 中每项有固定的 `id`（如 `system.wsl`、`install.units`），供脚本和将来的组件判断。
- **`status` 的提示**：`status` 末尾执行一次快速检查（不联网、不调用 `mihomo -t`），有 fail 时提示“N problems found; run tunlr doctor”。只看 fail：CPU 级别、UFW 等长期存在的 warn 每次都提示只会让人忽略。
- **菜单**：“Diagnose”在“Maintenance”分类中；未安装时的第一级也有。

## 5. 技术选型

### 5.1 为什么是 Go 单文件，而不是 sh

sh 的优势（轻量、随处可跑、可随手改）服务于路由器和老旧固件，本项目已放弃这些平台；而配置合并、错误处理与回滚在 sh 里难写对，还要依赖 yq、jq 和各种 shell 方言。Go 单文件只依赖 `nft` 和 systemd，YAML 按节点处理，事务逻辑有单元测试。sh 只留在安装脚本。代理变量不用 shell 函数：子进程改不了父 shell 的环境，`tunlr proxy env` 只输出语句由用户 `eval`。

不选 Rust：省下的几 MB 相对 17–19 MB 的内核不重要，Go 的 `yaml.Node` 对保留顺序和注释的合并支持更好。

### 5.2 为什么不做全屏 TUI

没有任何功能必须用全屏界面：向导、确认、多选用 `huh` 在普通终端完成，实时日志和流量交给 zashboard。全屏的坏处：tunlr 的操作是一次性事务，输出要往回翻、复制、贴给别人，备用屏幕退出就没了；出问题时内联输出停在最后一行，全屏会冻在半个画面；VM 控制台、串口、窄窗口里更容易乱。约束见 `CLAUDE.md`。

### 5.3 为什么不常驻

| 需求 | 实现 |
|---|---|
| 定时更新 | `tunlr-update.timer` 每小时唤醒 `_update`，按各自周期执行到期的更新（第 7 节“定时任务的治理”） |
| 内核崩溃后恢复与回滚 | `OnFailure=` 触发 `_rescue` |
| 开启 TUN 后的防失联回滚 | `systemd-run --on-active=` 临时定时器 |
| 规则丢失检测、`nftables.service` 清空后补回 | `tunlr-check.timer`、`tunlr-firewall.service`（8.4） |
| 面板升级的善后 | `tunlr-aftercare.path`（第 7 节） |
| 状态与漂移 | `status` 现场计算 |

好处：零常驻内存、故障点少、升级只需替换文件、日志由 journald 按 unit 归档。

### 5.4 依赖清单

见 `CLAUDE.md`。补充理由：

- **YAML 合并基于 `yaml.Node`**：保留键顺序和注释，diff 干净；不必为 mihomo 的大量字段定义结构体。
- **nftables 用 `nft -f`，不用 `google/nftables`**：模板写的是 nft 原生语法，生成的脚本可读、可手动执行、易于 golden 测试，`config plan --full` 原样给用户审查，`nft -f` 整个文件一个事务。netlink 库生成的字节表达式与 `nft list ruleset` 对不上，用户无法审查和复现。查询也用 `nft -j`。
- **diff 自己实现**：锚定（patience）逐行 diff。字符级的 diff-match-patch 差别大时要算几秒；锚定算法 O(n log n)，对每行多半唯一的配置也更易读。
- **正则用 `dlclark/regexp2`**：与 mihomo 筛选节点所用的相同，节点筛选的正则要原样写给内核，两边必须一致。

### 5.5 磁盘布局

| 路径 | 内容 | 说明 |
|---|---|---|
| `/etc/tunlr/` | `mixin.d/`（订阅 mixin 在 `mixin.d/<订阅名>/`）、`settings.yaml`、`subscriptions.yaml`（含机场凭据） | 用户的意图，可手改，备份的主体 |
| `/var/lib/tunlr/state.json` | 实例 ID、抓取记录、已知可用内核、TUN/网关等开关、定时任务的执行情况、查到的最新版本 | 0600，只存最新状态，不存会增长的历史 |
| `/var/lib/tunlr/subscriptions/` | 各订阅最近一次成功抓取的内容 | 含机场凭据 |
| `/var/lib/tunlr/history/` | 已发布配置的修订（配置 + 记录） | 保留 10 个，用于回滚 |
| `/var/lib/tunlr/backups/` | `backup restore` 前的自动备份 | 保留 3 份 |
| `/var/lib/tunlr/kernels/<版本>/` | 已知可用的内核 | root 所有，保留 3 个 |
| `/var/lib/tunlr/bin/mihomo` | 运行中的内核 | 归内核用户 `tunlr`，面板升级时原地替换 |
| `/var/lib/tunlr/core/` | 内核工作目录：生效配置、`api.sock`、geo、面板 | 归 `tunlr`，0750 保护 API socket |
| `/etc/systemd/system/tunlr-*` | 工具生成的 unit | 每次 `init` 重写 |
| `/etc/sysctl.d/90-tunlr-gateway.conf` | 网关的 `ip_forward`（8.10） | 只在网关开启时存在 |
| `/run/lock/tunlr.lock` | 文件锁 | 不删除，否则并发者会锁住不同的文件 |

- `/var/lib/tunlr` 为 0711：内核用户只能进入自己的 `bin/`、`core/`，读不到订阅、历史和状态。
- 所有路径取自 `paths.Layout`，测试时用 `Root` 指向临时目录；写入一律经 `fsutil.WriteFile` 原子替换。
- 状态文件比工具新则拒绝并提示升级（发布前不写迁移）。
- **`/etc/tunlr` 与状态文件的分工**：`/etc/tunlr` 放意图（可手改，下次抓取或 `config apply` 生效）；状态文件放工具观察到的事实，以及牵涉 nft、sysctl 或防失联检测、手改会绕过它们的开关（TUN、网关、局域网访问）。
  - 订阅的定义和 `active` 是意图，在 `subscriptions.yaml`：不放 mixin（mixin 描述合成出的配置，订阅来源用不上它的合并语义），不放 `settings.yaml`（设置只由用户写，`sub use` 每次都写 `active`）。
  - API secret 是工具生成的凭据，留在状态文件。设备与过滤项也是意图，暂在状态文件，以后再移。
- 工具设置：文件不存在用默认值，未知的键报错（避免拼写错误被静默忽略）。

## 6. 配置流水线

```
模板 + 订阅 + 受管 mixin + 用户 mixin → 合成（用户 mixin 动了受管的键时拒绝）
  → mihomo -t → 临时文件 + os.Rename → API 热重载 → 失败恢复上一版 → 记录历史
```

- **订阅是机场提供的完整配置**：机场的价值在它维护的策略组和分流规则，只取节点不够。工具自己抓取，失败时保留上次成功的结果。只含节点的订阅（模板提供组和规则）留作将来的第二种来源，共用同一条流水线。
- **可以保存多个订阅，同一时间只启用一个**，不合并。`sub use` 本身是一次经校验、失败回滚的发布；不能删除启用中的订阅。共用 mixin 引用某机场特有的组时，`patch`、`delete` 找不到只警告，`-t` 不通过则不发布。
- **订阅抓取**（`pkg/core/subscription`）：
  - 机场按 User-Agent 决定格式，默认值同时含 `clash.meta` 与 `mihomo` 子串以命中面板的常见判断。优先级：单个订阅 > `settings.yaml` > 内置；拒绝控制字符和首尾空白（防 HTTP 头注入）。
  - 内容必须是带节点的 YAML 映射，否则算失败；限制大小和时长。
  - **URL 常带令牌：输出、日志、错误中只出现 `scheme://host/...`**，包括 net/http 错误里的 URL。订阅内容文件 0600、目录 0700。
- **订阅列表**（`/etc/tunlr/subscriptions.yaml`，0600）：允许手改，工具基于 `yaml.Node` 只改变化的值以保留注释和顺序；读取时严格校验（未知键、重复名、`url`/`path` 不是恰好一个等都报错，错误中不含地址）。
  - 手写的订阅没有抓取记录，视为立即到期。
  - 名称添加后不能改：它是 `mixin.d/<名称>/`、内容文件名和 `config edit <名称>/…` 的一部分。规则同 mixin 目录名，不以 `.yaml` 结尾（免与共用 mixin 同名），不区分大小写地唯一。
- **订阅的来源**：地址（只认 http/https）、本地文件或标准输入。文件和标准输入的内容也复制到 `subscriptions/`，源文件删了照常可用。文件默认不自动更新（只有用户自己改它）；标准输入（文件在自己电脑上，`ssh host tunlr sub add x - < a.yaml`）不能自动更新。换来源、换 User-Agent 都是抓取成功才保存。
- **分层合成，各层职责固定**（`pkg/core/config`）：
  1. 模板：系统部分（端口、面板、profile、TUN），再合并兜底值 `tunlr:defaults`、用户的 `tunlr.defaults`。
  2. 订阅：**只取白名单中的键**，其余丢弃并在 plan 列出：订阅不可信，不能开端口、改监听地址或设 API。白名单为路由（`proxies`、`proxy-groups`、`rules`、各 providers、`sub-rules`）、DNS（`dns`、`hosts`、`sniffer`，整段采用，因为机场规则常与它的 DNS 配合）、geo（`geox-url` 等，规则要用它指定来源的数据）。采用的顶层键整个替换兜底值，`geox-url` 按文件替换，未写的文件不退回内核默认地址（不经镜像时国内下载不了）。
     - 例外：订阅的 `dns.listen` 一律丢弃（决定 DNS 是否暴露），由 `tunlr:dns` 设为回环。开 TUN 时 `tunlr:tun` 独占 `dns.enable`。
     - 订阅的 DNS 为桌面客户端而写，合成后对明显问题（如 `dhcp://en0` 指向不存在的网卡）只警告。
     - 然后执行节点筛选。
  3. 默认值类受管 mixin（`tunlr:dns`、`tunlr:mirror`、`tunlr:filter`）。
  4. 用户 mixin：共用的，再启用订阅的。
  5. 独占类受管 mixin（`tunlr:core`、`tunlr:tun`、`tunlr:panel`、`tunlr:proxy`）。
  6. 改写：设了镜像时把 `geox-url` 中 github.com 的地址改为经镜像，不论来自哪层，订阅自定义的来源也能在国内下载。
- **tunlr 对配置的改动只有三条途径**，每处都能在 `config plan` 或 `tunlr config` 中看到出处；新功能必须归入其一，不在合成代码里写特例：模板（基底）、合成规则（过滤不可信输入，如白名单）、受管 mixin（交互功能、兜底值、工具运行所需）。
  - 命名空间按操作类型划分，不按谁写：`mihomo:` 写值，`tunlr:` 是 mihomo 写法表达不了的操作。按“程序/用户”划分就要在 `tunlr:` 下重造写值语法。
  - 镜像改写是值的变换，不为它扩充 mixin 语法，作为受管 mixin 的附加项登记。
- **受管 mixin：mixin 是统一的底座**。交互功能都是对配置的修改：开关存在状态或设置中，写成什么由受管 mixin 表达，与用户 mixin 走同一引擎（`pkg/core/config/managed.go`）。
  - 独占类（`tunlr:core`、`tunlr:tun`、`tunlr:panel`、`tunlr:proxy`）在用户 mixin 之后；用户 mixin 写了它的键（即使值相同）、用 `null` 删、或整个替换其上级，报错不发布（退出码 3），指出文件、行和该用的命令；同一映射的其他键照常可改。
  - 默认值类（`tunlr:defaults`、`tunlr:dns`、`tunlr:mirror`、`tunlr:filter`）在用户 mixin 之前，允许覆盖，plan 记为覆盖。放在订阅之后，因为插入规则要订阅的 `rules` 就位、`dns.listen` 要补在订阅的 `dns` 上。
  - 拒绝而不是忽略：被忽略的设置让人以为生效了。代价：升级新增独占键后，已写了它的 mixin 会让定时更新保持当前配置，`doctor` 列出。
  - `secret` 不支持自定义，只能 `panel reset-secret`；任何列表都不显示它。
  - 只在内存中合成，不写入 `mixin.d`：写成文件会让人以为可编辑、改了又被覆盖。命令替用户写的文件（如 `<订阅>/10-filter.yaml`、`<订阅>/20-geo.yaml`）归用户所有。
  - 回滚和自动关闭透明代理不重新合成，只按当前状态重写独占的受管 mixin（含面板监听地址和 `secret`）。
- **mixin**（`pkg/core/config/mixin.go`）。参考 clashctl（单文件、嵌套操作）和 ShellCrash（分文件、`-t` 失败静默丢弃全部自定义）。
  - **两个命名空间：`mihomo:` 是原生配置，`tunlr:` 是 tunlr 的语法**。顶层只能有这两个和放锚点的 `x-`：

    ```yaml
    mihomo:
      log-level: warning
    tunlr:
      rules:
        prepend: [DOMAIN-SUFFIX,corp.example.com,家宽]
        delete: [/^GEOIP,CN/]
    ```

    - `mihomo:` 的键直接查 mihomo 文档，是兜底；不全用 `tunlr:` 建模，否则要永远追上游。`tunlr:` 的操作只针对节点、策略组、规则：只有订阅带来的这三个列表需要在机场内容上增删改。
  - **mixin 的兜底值：`tunlr.defaults`**：写法同 `mihomo:`，但在订阅之前合并，只在订阅没写时生效（如自己的 geo 地址、订阅不带 DNS 时的 DNS）；写了独占键不报错。放在 mixin 而非工具设置：它改的是配置，工具设置只放工具行为。用通用的 `defaults` 而非专用 `geo`：会被订阅整段替换的只有 DNS 和 geo，白名单加键不用加语法。
    - **tunlr 的兜底值是受管 mixin `tunlr:defaults`**（`templates/defaults.yaml`），不在模板中：模板里看不出值是兜底还是系统设置，作为受管 mixin 能逐键报告来源。随 tunlr 升级更新。
  - **`mihomo:` 的合并语义**：RFC 7386 JSON Merge Patch，映射逐层合并，标量和列表整体替换，`null` 删除。列表只整体替换：要增删的只有订阅的三个列表，归 `tunlr:`。整体替换 `proxies`、`proxy-groups`、`rules`、`sub-rules` 时警告（订阅更新后机场内容全丢）。
  - **`tunlr:` 的操作**：`proxy-filter`，以及对 `proxies`、`proxy-groups`、`rules`、`sub-rules.<名称>` 的 `prepend`、`append`、`patch`、`delete`；其他键报错。
    - 规则 `append` 插在末尾 `MATCH` 之前，否则永不命中。
    - `patch` 按 `name` 递归修改；列表字段用显式的 `prepend-<字段>` 等，不靠值类型猜。找不到只警告：机场改名很常见。
    - 删节点或组时同时从所有组的 `proxies` 移除。名称可写 `/正则/`。
    - providers 是映射，`mihomo:` 逐键合并已够，不设操作。
  - **一个文件内的执行顺序**固定：`mihomo:`，再 `tunlr:` 的删除 → 修改 → 插入。这样能先删订阅的 `MATCH` 再追加自己的，删信息节点不误删自己加的，`patch` 能改到 `mihomo:` 刚替换的内容。`proxy-filter` 在任何 mixin 之前。
  - 锚点不能跨文件；mixin 的注释不带入配置。
  - **正则与 mihomo 相同**：都用 `dlclark/regexp2`。同一字符串既由 tunlr 执行又原样写给内核，结果必须一致。
- **节点筛选（`tunlr.proxy-filter`）**：mihomo 的筛选只在 provider 和策略组上，且组的 `filter` 对按名称列出的节点不生效；机场配置绝大多数是顶层列节点、组按名称引用，原生筛选去不掉，所以由 tunlr 实现。
  - 字段和语义照搬 mihomo 的 provider（`filter`、`exclude-filter`、`exclude-type`，依次执行）：它决定“有哪些节点”，`exclude-type` 匹配配置写的 `type`。
  - 只筛订阅带来的节点（在订阅层之后、任何 mixin 之前），mixin 追加的不受影响；订阅 mixin 逐字段覆盖共用的。
  - 直接列出的节点：去掉的从所有组移除，因此变空的组也删掉（内核会给空组放兜底出站，但已无可用节点）。被删的组仍被规则引用、或节点全被筛掉时报错，不悄悄改成直连或拒绝。
  - `proxy-providers`：把字段写进每个 provider 由内核筛选；两个 `filter` 都要满足，用前瞻组合（只有 regexp2 支持）。
- **多个 mixin 文件**：`mixin.d/*.yaml` 按文件名排序**逐个应用**，每个作用在前一个的结果上，后者优先。只读 `.yaml`，改名 `.yaml.off` 即停用。
  - 逐字符排序，同 `ls`、`run-parts`、systemd drop-in，不做自然排序：`ls` 顺序就是应用顺序。位数不同的前缀出乎意料（`9-a` 在 `10-b` 之后），plan 警告并建议补零。
  - **订阅 mixin**：`mixin.d/<订阅名>/*.yaml`，排在共用的之后，只应用启用订阅的，用于只对一个机场有意义的修改。
    - `sub add --include/--exclude` 是快捷方式，写入 `<名称>/10-filter.yaml`；不另设 `sub filter` 命令：筛选就是 mixin 的能力。
    - `sub set` 是修改一个订阅的唯一入口。
    - `sub remove` 一并删除，备份与还原包含它。编辑未启用订阅的 mixin 时按“它被启用”合成并 `-t`，只保存不发布。
  - mixin 可能含节点密码：目录 0700，文件 0600。
- **范围与检查**：报错不发布（退出码 3）的包括 YAML 错误、顶层未知键、动了受管的键、`tunlr:` 未知键、正则无效、端口安全不通过、`-t` 不通过；排序意外、整体替换订阅列表、`patch`/`delete` 无匹配只警告。
  - **端口安全**（`pkg/core/config`）：结果会让其他主机无需凭据使用代理或 DNS 时不发布，避免成为开放代理、开放解析器。覆盖 `allow-lan` 无认证且来源不限于私有地址段、`listeners` 监听非回环且无认证、`dns.listen` 不在回环（反射放大）、`external-controller` 无 `secret`。注意内核的默认值是放开的：不写 `lan-allowed-ips` 即放行所有来源，`listeners` 不写 `listen` 即 `0.0.0.0`。
    - 只检查重新合成的配置；回滚、自动关闭 TUN 不检查：拦下发布过的内容只会让恢复失败。
    - 凭据由用户写在 mixin 里，tunlr 不生成、不保存。`doctor` 对生效配置做同样检查。
  - mixin 有错时绝不丢掉它去发布（ShellCrash 的做法）：丢掉直连或拦截规则会让流量走错路。定时更新遇到此类错误保持当前配置。
- **`tunlr config edit [名称]`**：同 visudo、`kubectl edit`，编辑临时副本，保存后合成并 `-t`，有错让用户重编或放弃，**通过后才写回 `mixin.d`**（定时任务读不到有错的编辑），再 diff、确认后发布。
  - `-t` 因下载失败未完成时可只保存不发布；编辑期间文件被别人改过时不覆盖。
  - 编辑器取 `$VISUAL`、`$EDITOR`，否则 `vim`、`nano`、`vi`，不经 Debian 的 `editor`（常指向 nano）。
- 合成前展开订阅的锚点与别名（被引用的锚点可能在被丢弃的键里），并限制展开后的节点数，防恶意订阅放大。
- **校验时的 geo 数据**：tunlr 不下载 geo 数据（第 7 节），交给 `-t` 按 `geox-url` 现场下载。
  - `-t` 在临时工作目录运行，已有 geo 文件以符号链接提供：`-t` 可能写缓存，直接用内核工作目录会与运行中的内核冲突。需要下载时放宽时限；下载到的文件存进内核工作目录，校验没通过也保留。
  - **geo 来源变化**：geo 文件所有订阅共用（内核只认固定文件名）。换了 `geox-url` 后旧文件要等过了更新间隔才重下，新规则引用的分类在旧文件里没有。所以 plan 比较来源（只换镜像不算变化），变了的不提供旧文件，由 `-t` 下载到暂存目录；apply 核对暂存文件仍是计划时的，换入后重启内核（geo 只在启动时加载），失败时文件与配置一起换回。
  - 依赖下载失败不报告为配置错误（不映射到退出码 3），保持当前配置。`-t` 不抓远程 providers，它们不可达不影响校验。
- **内核自己的下载**：tunlr 只下载内核（官方 sha256 校验）和订阅（无从校验）。面板、geo、providers、面板触发的内核升级都由内核发起，走内部 tunnel 按规则分流，失败才直连；策略组选中坏节点时面板和 geo 下载全部失败。
  - **镜像不加直连规则**：不插入 `DOMAIN,<镜像域名>,DIRECT`：这是配置而不是工具行为，归 mixin；节点失效时在面板上切节点即可。
  - **信任边界**：镜像能篡改转发的内容；工具的下载有官方 sha256 兜底，内核的没有。
    - 面板交给内核（`external-ui` 为空时按 `external-ui-url` 下载）：之后面板上的更新都不经 tunlr，tunlr 只校验第一次安装是不存在的保证。
    - geo 数据不校验：内核每次更新都不校验，事后核对只能在用过之后发现；订阅自定义的来源也无从校验。
    - 选用可信的镜像是用户的事。
- **充分利用内核已有能力**，模板默认开启：`external-ui`/`external-ui-url`、providers 的 `interval`、`profile.store-selected`/`store-fake-ip`、`ntp` 等。
- 工具经 `external-controller-unix` 访问 API，写为相对路径 `api.sock`（临时目录中的 `-t` 不受影响）。内核以 0666 创建它且不要 secret，只能靠工作目录 0750 挡住其他本地用户，每次发布都收紧。面板走只监听本机的 `external-controller`，由 secret 保护。
- **发布分两步：预览（plan）与发布（apply）**（`pkg/core/pipeline`）：
  - plan 合成、diff、`-t`，不改状态（只可能留下 `-t` 下载的 geo）。
  - apply 确认期间不持锁，避免阻塞 timer 任务；加锁后核对生效配置与预览时一致，否则要求重新预览。非交互必须 `--yes`。
  - 内核加载失败时恢复原配置，退出码 4，失败的配置不进历史。生效配置被手改过时，发布前先存为一个修订，免得丢失。
  - **节点变化摘要**：diff 前一行汇总节点的改动和改名（一减一增除名称外相同即改名）。只列字段名不显示值（密码）。原因：机场每次抓取会给部分节点换端口、信息节点名带剩余流量，只看 diff 分不清“端口变了”还是“换了节点”。
  - **长 diff 按段汇总**：过长时终端按顶层键汇总，`--full-diff` 看全文；`--json` 总是完整。
  - **长行截短**：订阅常把策略组写成一行，终端显示时截短超宽行，保留行首和改动处。不改写成每项一行：发布的配置要保持机场的写法。
- **emoji 原样输出**：yaml.v3 把基本平面外的字符写成 `\U...` 转义，编码前后用私用区字符替换回避（`config/encode.go`）。
- **大订阅的内存**：tunlr 常跑在小内存设备上，大订阅（1 万节点）plan 时内存可达数十 MB：
  - 分段编码：yaml.v3 编码器留住整份事件，约为配置的 45 倍；按顶层键和长列表分段编码，结果与整体编码逐字相同（有测试）。
  - 每份配置只解析一次；订阅统计在抓取时记录，列出订阅时不解析内容。
  - 默认 Go 堆软上限 64 MB（`GOMEMLIMIT` 可覆盖）。
  - 剩下的大头是节点树（约订阅的十倍），要再降牵涉 mixin 合并，暂不做。
- **热重载**（`pkg/core/mihomo`）：经 API socket `PUT /configs?force=true`，再 `GET /version` 确认仍在响应。
  - 不带 `force=true` 时端口等监听设置的变化不生效；204 只表示接受了配置，所以要事后确认。
  - 重载同步初始化 providers，可能较慢，设时限；超时附上最近的非连接日志（连接日志很快冲掉卡住时那段）。
  - socket 不存在或连接被拒视为内核没运行：照常发布，启动时加载。但重载后才不响应的，即使随后连接被拒也回滚，让下次启动用回原配置。
  - 热重载不重建 API 服务、不重新注册 geo 定时器，所以 `external-controller*`、`secret`、`external-ui*`、`tls`、`geo-auto-update`、`geo-update-interval` 等变化时 apply 改为重启内核，plan 预先说明；重启失败则恢复原配置再重启，退出码 4。
- **回滚**：不带号时回到最近一个与生效配置不同的修订，连续两次即来回切换（同 `helm rollback`）；也可指定修订号。同样经过 `-t` 和确认，记为新修订，修订号不复用。恢复后按当前状态重写独占的受管 mixin。回滚只恢复配置文件，下一次 apply（含订阅更新）按当前订阅和 mixin 重新合成。

## 7. 内核管理与救援

- **内核自升级的实际行为**（据 v1.19.33）：面板经 `POST /upgrade` 从官方 release 下载，**不做任何校验**；在二进制所在目录原地覆盖（所以内核用户须能写 `bin/`）；按编译时的 GOAMD64 选资产；替换后 `syscall.Exec` 原地运行，PID 不变，systemd 看不到重启，新版本起不来时才进入救援。
- **按 CPU 级别选构建**：标准 amd64 构建要求 x86-64-v3，通用 CPU 型号的虚拟机常只有 v1。按 `/proc/cpuinfo` 选带级别的 `mihomo-linux-amd64-v1|v2|v3`（与面板升级时内核自选的一致）；`doctor` 报告级别，低于 v3 时建议改 CPU 型号。
- **版本**：默认最新正式发布，不支持 alpha。
- **镜像下载，官方校验**：国内服务器直连 GitHub 很慢，可配置下载镜像；但**校验值只取自 `api.github.com` 的资产 sha256**，镜像只传字节，不一致则丢弃，没有 sha256 的资产拒绝下载。
- **续传与重试**：慢速线路常中途断开，所以不设总时限，只看有无进展，断开后带 `Range` 续传；完整性由官方 sha256 兜底。
- **API 不经镜像**：发布信息只直连 `api.github.com`。理由：未认证限额按 IP 计，镜像出口 IP 被所有用户共用，基本总是用完；sha256 也不能经镜像的手。`install.sh` 同样如此。
  - **API 限流**：用完时报告限额和解除时间。正常一天只查几次，只有反复测试或多机共用 NAT 才会用完；`github-token` 设置未定。
- **运行身份与权限**：内核以固定系统用户 `tunlr` 运行，只授予 `CAP_NET_ADMIN`、`CAP_NET_BIND_SERVICE`；不用 `DynamicUser`，因为工作目录要由 root 写配置、归内核用户所有，动态 uid 不固定。为让面板升级可用，`bin/` 和工作目录归 `tunlr`。
  - **root 不执行内核用户可写的二进制**（否则即提权）：`mihomo -t` 降权运行。
- **已知可用版本**：root 所有的副本保留最近几个，用于回滚和救援。只有 sha256 与官方一致且健康检查通过的版本才记入；面板升级来的由善后核对后记入。
- **健康检查**：启动后 API 返回预期版本，且服务稳定、未重启。
- **工具升级内核**（`tunlr core upgrade`）：下载校验 → 降权用新内核对生效配置跑 `-t`，不通过则停止、运行中的内核不受影响 → 加锁替换 → 重启并健康检查，失败换回原二进制（退出码 4）→ 记为已知可用。`core rollback` 换到上一个已知可用版本，同样经过 `-t` 和健康检查。
  - **离线安装**：`--file`（`init --kernel-file`）给出官方资产，按官方资产名得知版本和平台：平台须为本机，amd64 级别不高于本机 CPU（否则非法指令崩溃）。仍查该版本的官方 sha256，查不到就拒绝、不提供跳过参数——下载慢或被阻断的离线场景恰恰最需要校验。
- **面板升级的善后**（`_aftercare`）：`tunlr-aftercare.path` 监视 `bin/mihomo`，文件替换时触发。等锁（工具自己的替换在锁内已确认，善后见到已知可用就结束）；否则等新内核真正运行（`/proc/<MainPID>/exe` 与 `bin/mihomo` 相同）后再查 `/version`——内核先换文件后 exec，过早查到的是旧版本，会误判为篡改——然后下载该版本官方资产比较：一致则记为已知可用，不一致或下载不到（如 alpha）就换成校验过的版本。不挂在 `_update` 上：与订阅无关，最多要等一小时，期间跑的是未校验的内核。
- **救援**：`tunlr-core.service` 失败重启到上限后经 `OnFailure` 执行 `_rescue`。透明代理规则已由 `ExecStopPost` 清理（见 8.4），SSH 不受影响。当前二进制不是已知可用时换回最近的已知可用版本；已是已知可用仍起不来，多半是配置或环境问题，保持停止并记录原因，不循环重启。`core upgrade` 持锁自行回滚，`_rescue` 拿不到锁直接退出。
  - 代价：面板升级出问题时有短暂断网窗口，工具升级没有。README 说明这一取舍。
- **unit 的其他设置**：沙箱选项（`ProtectSystem=strict`、只读写 `bin/` 与 `core/` 等）下 TUN 正常工作（见 8.2）；`ExecStartPre`、`ExecStopPost` 见 8.4。
- **定时任务的治理**：按“该不该由用户决定”分三类：
  - 护栏（救援、TUN 回滚定时器、`tunlr-check.timer`、`tunlr-firewall.service`、善后）：不能关闭，属于“应用 → 确认，失败则回滚”。
  - 资源更新（订阅、geo 数据）：内容会过期，可改周期或关闭。
  - tunlr 自身升级：不定时，只手动 `tunlr upgrade`；`_update` 每天查一次 tunlr 与 mihomo 最新版本用于提示，不自动升级。
  - 只用一个 `tunlr-update.timer`，每小时唤醒，由 `_update` 按状态文件判断到期项。相比每种资源一个 timer：改周期不重写 unit、不 `daemon-reload`；下次时间由工具算，可直接显示；各更新顺序执行；错过的开机后补上。代价是不能指定时刻，服务器可接受。timer 总是启用，因为随时可能添加订阅。
  - **订阅**：周期是订阅的属性。未启用的订阅也按周期更新（切换过去时内容是新的），只有启用的订阅成功更新才发布；失败保留上次内容，较快重试。
  - **geo 数据**：周期属于配置（`geo-update-interval`），由内核自己执行，`_update` 从不视其到期；`tunlr update run geo` 经 `POST /configs/geo` 立即更新。见下文“geo 数据更新”。
  - 某步失败记日志并继续，最后非零退出让 systemd 记为失败。
- **geo 数据更新**：
  - 事实：geo 数据只能由内核自己的更新器重新加载，外部替换文件须重启内核；内核下载后不校验。
  - 设计：tunlr 只下载管理内核和订阅（面板同样交给内核，见第 6 节“信任边界”）；geo 数据由内核按配置下载和更新，tunlr 不下载、不校验，`init` 也不预下载（`mihomo -t` 时内核自己下载），不事后核对——内核每次更新都不校验，发现时篡改文件已被使用，订阅自定义的来源也无从校验。
  - 订阅可指定自己的来源（规则依赖它），所以订阅写了的为准；tunlr 只在 `tunlr:defaults` 中兜底（每周、官方 meta-rules-dat，见第 6 节“tunlr 的兜底值”），`geox-url` 按文件替换。设置了镜像时，`geox-url` 中 github.com 的地址最后改为经镜像。
  - **按订阅调整**：`sub set <订阅> --geo-update/--geo-url` 写入该订阅的受管 geo mixin（见第 6 节“受管 mixin”），只在它启用时生效；排在它之后的用户 mixin 写了同一键时拒绝修改并指出该改的文件，免得改了不生效。菜单中 geo 是订阅的一项属性，不单列。只按订阅调整，因为 geo 数据服务于订阅的规则，订阅自带来源时改兜底无效；覆盖订阅的 mixin 结果确定，不必判断每个键来自哪一层。
  - 来源可见：`tunlr update` 和 `config plan` 列出每个键的值来自兜底、订阅还是 mixin。
- **状态以实际为准**：版本、TUN 状态等一律经 API 查询内核；工具的记录只作“已知可用的历史”。
- 内核 API 不稳定，调用集中封装在 `pkg/core/mihomo` 的 client 中，随 mihomo 发版做兼容性测试。

## 8. 透明代理

> 8.3～8.6 是本机模式的设计，8.10 是网关模式；8.2、8.7～8.9 是约束设计的实测事实。

### 8.1 方案：TUN + auto-redirect

- 开启 `auto-route`、`auto-redirect`，**关闭 `strict-route`**（它让本机无法被其他设备访问）。**不用 `route-exclude-address` 排除私有地址段**：直连网段本来不进 TUN，整段排除反而让局域网 DNS 服务器绕过劫持（8.2）。
- 理由：auto-redirect 用 nftables 处理 TCP，避开 TUN 回包被 rp_filter 丢弃，且性能优于 tproxy。工具只补内核规则缺的几条（8.4）。
- 局限：auto-redirect 只接管 TCP，UDP 走 TUN 路由。
- 不实现 tproxy。

### 8.2 实测

- 热重载即可开关 TUN；内核以 `tunlr` 用户、只带 `CAP_NET_ADMIN` 即可建立 TUN、ip rule（9000～9010，表 2022）和 `inet mihomo`，正常停止时自行清理。
- 直连网段（本机网段、Docker 网桥）不进 TUN（`suppress_prefixlength 0`）。
- 排除私有地址段时 `inet mihomo` 的 `return` 早于 DNS 的 `dnat`，局域网 DNS 服务器的劫持失效。
- 绑定 0.0.0.0 且不指定回包源地址的 UDP 服务失败（源地址选成 198.18.0.1），可用 `ip rule sport <端口> lookup main` 解。
- Docker 发布的端口失败（容器回包是转发流量，进 TUN），由 8.4 的回包规则解决。
- 内核被 `SIGKILL` 后残留的 `inet mihomo` 把容器 TCP、DNS 重定向到不存在的端口，内核起不来时必须清理它。

### 8.3 开关与配置

- **开关**：`tunlr tun on/off`。`--mode` 预留给 tproxy。
- **开关存在 `state.json`**，不放 `settings.yaml`：手改设置会绕过防失联检测。状态 `off`、`pending`（检测中，带回滚期限）、`on`。
- **配置复用分层合成**：`tun on/off` 只改开关，再走 `config apply` 的流水线（`-t`、原子写入、热重载、失败回滚、历史）。
  - 模板给 `stack`、`dns-hijack` 默认值，mixin 可覆盖。
  - 受管 mixin `tunlr:tun` 独占 `tun.enable`、`auto-route`、`auto-redirect`、`strict-route`、`auto-detect-interface`、`exclude-uid`、`device: tunlr0`、`iproute2-table-index`、`iproute2-rule-index`；用户 mixin 写了就拒绝发布。工具的规则、清理、检测依赖这些值。
  - **防回环有两道，互不依赖**（sing-tun 的 auto-route 不设 `routing-mark`，内核自己的出站会回到 TUN）：
    - `auto-detect-interface`：内核出站绑定默认路由所在的非 TUN 网卡。
    - `exclude-uid`（内核用户的 UID，合成时查 `/etc/passwd`）：内核用户的包查主路由表，也不被 `oifname "tunlr0"` 的重定向命中。实测单靠它即可，两道都去掉则回环。
    - `doctor` 检查两道：都缺 fail，缺一道 warn。不检查内核出口网卡：`auto-detect-interface` 的绑定不反映在 `ip route get` 中，会误报。
    - 服务器自己跑全隧道 VPN（wg-quick）时 TUN 的规则更早，内核的连接绕过 VPN，VPN 握手包可能经过内核。
    - 约束：内核用户的流量全部绕过 TUN，它只运行 mihomo；将来需要经代理的组件用别的用户。
  - 不写进 `mixin.d`，理由见第 6 节“受管 mixin”。
- **回滚**：`config rollback` 后按当前开关重写 `tunlr:tun`，免得配置与工具规则、防失联状态不一致。
- **面板运行时切换**（3.3）：不写回配置、不经防失联检测；工具的规则不依赖开关，同样生效。`status` 提示不一致，`tun on/off` 以开关为准。

### 8.4 工具补充的规则及其生命周期

- 规则在独立的 `inet tunlr` 表，`nft -f` 原子加载，整表删除即清理：
  - **入站连接的回包走主路由表**：从 `tunlr0` 以外进入、目标为本机（`fib daddr type local`，尚未 DNAT）的连接打 conntrack 标记，回包按 fwmark 查主路由表（ip rule 优先级 8999，早于内核的 9000）。修好 Docker 发布的端口，也让入站不依赖内核规则细节。不限新连接，已建立的 SSH 下一个包即标记，所以 `tun on` 发布前先加载本表。
  - 标记只用 `0x10000000` 且按位设置，不覆盖 Tailscale、kube-proxy、auto-redirect 的标记。
- **与内核服务同生共死**，不依赖开关（TUN 关闭时规则无害，面板打开 TUN 也有效）：
  - `tunlr-core.service` 的 `ExecStartPre=-+… _prestart` 加载（`+` 以 root、不受沙箱；`-` 失败也启动内核）。
  - `ExecStopPost=-+… _poststop` 删除它们，以及残留的 `inet mihomo` 和 9000～9010 的 ip rule。进程以任何方式退出都执行，`_rescue` 不必单独清理。
  - 两者不取文件锁（`core start` 持锁调用 `systemctl start` 会死锁），本身幂等。
- **规则被他人清空**（重新加载 Debian 的 `nftables.service` 以 `flush ruleset` 开头；启动 firewalld 会停掉 `nftables.service` 并让 Docker 的链消失），两道补救：
  - `tunlr-check.timer`（每 5 分钟）执行 `_check`：按状态补齐网关（8.10）、`inet tunlr_access`（第 10 节）、`inet tunlr_devices`（8.12）、内核运行时的 `inet tunlr`；透明代理开着而 `inet mihomo` 不在时重启内核。兜底：`pending` 过期时关闭 TUN。
    - 补表时取文件锁：防止与 `_firewall` 同时补出重复规则（实测出现过），也防止与 `gateway off` 等交错补回刚删的表。
    - timer 在透明代理、网关、局域网访问任一开着时启用；都关闭时 `_check` 停用自己。面板临时打开的 TUN 不在保护范围内。
  - `tunlr-firewall.service`（总是启用）：`PartOf=`、`ReloadPropagatedFrom=nftables.service`，后者启动、重启、重新加载后执行 `_firewall` 立即补表，不重启内核。`systemctl stop nftables` 后的 `start` 拉不起它，由 `_check` 拉起。持锁的操作先释放锁再同步 unit（`enable --now` 会等 `_firewall`）。
  - `tunlr-core.service` 排在 `nftables.service` 之后，开机加载的表不会被清空。
  - Docker 晚于内核建好 `DOCKER-USER` 时由 `_check` 补放行规则，不另设随 Docker 启动的单元：网关的 sysctl 文件让 `ip_forward` 开机即为 1，Docker 此时不会把转发策略设为 drop。

### 8.5 开启流程与防失联

`tun on` 的顺序即设计：

1. 前置检查（退出码 3）：内核在运行；`/dev/net/tun`、`nft`、`ip` 可用；8999～9010 没有他人的 ip rule。有默认丢弃入站、又不先放行 DNAT 连接的链（如 UFW）时警告：内核重定向的 TCP 会被丢弃。
2. 显示 `config plan` 并确认。
3. 加锁，开关记为 `pending`（期限 3 分钟），用 `systemd-run` 布置执行 `_tun-guard` 的回滚定时器。
4. 加载工具的规则，发布配置。发布失败则流水线回滚、开关复原（退出码 4）。然后**释放锁**，卡住的检测不挡住回滚定时器。
5. 检测：TUN 与三者（`tunlr0`、`inet tunlr`、`inet mihomo`）都在；系统 DNS 能解析；测试地址至少一个可达（全部失败才算失败）；**SSH 仍通畅**（看本次 SSH 连接的 `bytes_acked` 是否增加；SSH 断开则进程因 SIGHUP 退出，交给回滚定时器）。
6. 全部通过记为 `on`、停定时器；任一失败立即关闭，退出码 4。

- **重启机器**：`systemd-run` 的临时定时器不保留，所以 `_prestart` 也检查 `pending` 过期，先关闭 TUN 再启动内核（只尝试加锁、不等待）。
- `tun off` 不做检测。
- 不需要用户手动确认：SSH 入站本来不受影响，加上 8.4 的回包规则，失联只是兜底。

### 8.6 与其他功能的关系

- `config apply`、`_update`、`sub use` 照常合成，`tunlr:tun` 随开关写入，定时更新不改变 TUN 状态。
- `uninstall`：清理规则，删除 `tunlr-check.timer`、`tunlr-firewall.service` 和所有 tunlr 表；开关记为 `off`，重新 `init` 后 TUN 保持关闭。
- 代码在 `pkg/core/netfilter`（规则、前置检查）与 `pkg/core/setup`（开关、流程）；nft 模板做 golden 测试，规则在 network namespace 中做集成测试。

### 8.7 实现后的实测

- 规则重复加载幂等；`_poststop` 清理内核残留、保留他人的 ip rule。
- 开启前已建立的 SSH 能通过检测；丢弃 sshd 回包时检测失败并立即关闭。
- 检测阶段进程被杀、回滚定时器丢失两种情形都能关闭 TUN。

### 8.8 尚未实测

- 真实 UDP 服务（WireGuard、hysteria2 服务端）：受影响时再加 `ip rule sport <端口> lookup main`。
- IPv6：TUN 没有 IPv6 地址，IPv6 流量可能不经 TUN。
- UFW、firewalld 下容器的 TCP：与网关的局域网 TCP 同路，推断 UFW 丢弃、firewalld 放行；所以警告按链判断，不按程序名。
- systemd-resolved（`127.0.0.53`）、本机 Tailscale（ip rule 52xx）：按原理不受影响。

### 8.9 网关模式实测（MVP 5 设计前，设计见 8.10）

- `inet mihomo` 对局域网流量：DNS 被 `dnat` 到 `198.18.0.2`（成为转发流量）；其余 TCP 被 `redirect` 到内核本机端口（成为入站）；UDP、ICMP 路由进 `tunlr0`（经转发链）。
- Docker 把转发策略设为 drop 时，只有 IP 访问的 TCP 通过；在 `DOCKER-USER` 中加 `-i <局域网网卡> -o tunlr0` 及反方向的 ACCEPT 后全部通过。**在自己的 nft 表中 accept 无效**：同一 hook 上任何一张表 drop 就丢包。
- 结论：网关只缺 `ip_forward` 和转发被 drop 时的放行；不需要 NAT，不改 `dns.listen`。
- 尚未实测：旁路由（设备与网关同网段）、IPv6。

### 8.10 网关模式（MVP 5）

局域网设备把这台服务器设为默认网关和 DNS，流量经 mihomo 分流。只需补：IPv4 转发、转发被 drop 时的放行、内核不在时不让局域网流量直接出去。

- **命令**：`tunlr tun gateway on [--interface <网卡>]`、`off`。`init` 选“当网关和代理”时在内核运行后走同一流程；没有订阅时不问（内核不启动，无法检测）。
- **开关**存在 `state.json`（局域网网卡、开启前 `ip_forward` 的原值），理由同 8.3。
- **局域网网卡**：默认取 IPv4 默认路由所在的网卡（旁路由时与上行同一块），不能是 `lo`、`tunlr0`。是否接着真实网络按 `/sys/class/net` 判断（网桥等顺着下层找硬件网卡），不按网卡名猜。
- **只做 IPv4**。
- **`tun gateway on`**：TUN 没开时先走 `tun on` 的完整流程，失败则不开网关。加锁后：
  - 写 `/etc/sysctl.d/90-tunlr-gateway.conf` 开启 `ip_forward`（持久，开机即生效）。
  - 加载 `inet tunlr_gateway`：从局域网网卡进来、不出 `tunlr0`、目标不是私有地址段的转发流量丢弃，**内核停止、崩溃或面板关闭 TUN 时局域网断网而不是悄悄直连**（失败时关闭）。去往私有地址段（第 10 节“私有地址段只定义一处”）的照常转发：它们本来不经代理。
  - 有 `DOCKER-USER` 链时插入两条放行规则（注释 `tunlr`）。**这是项目中唯一调用 `iptables` 的地方**，没有这条链时不调用。
  - 任一步失败则撤销已做的，退出码 4。
  - 不做防失联检测：网关不改变本机自己的流量。
- **`tun gateway off`**：删除上述三项；开启前 `ip_forward` 为 0 且没有 Docker 时恢复为 0（有 Docker 时保持 1）；TUN 保持开启。
- **生命周期**：
  - `_poststop` 不删网关规则，内核不在时仍是失败时关闭。
  - `_prestart`、`_check`、`_firewall` 在开关开启时补齐（8.4）。
  - 局域网网卡总是被代理端口和面板的访问限制放行（第 10 节“对其他设备开放的端口”）。
  - `tun off` 与防失联回滚关闭 TUN 时，先在同一把锁中关闭网关。
- `--json` 时要求 TUN 已开启：两段流程的 JSON 输出无法合并。
- 再次 `gateway on` 补缺；换网卡时先撤旧网卡的规则，`ip_forward` 原值仍按最初记下的。

**其他防火墙**：同一 hook 上的基础链按优先级依次执行（nft 与 iptables-nft 一样），任何一条丢弃即丢弃，accept 只是交给下一条，所以工具无法替别的防火墙放行，只能发现并提示。局域网流量分三条路：`dns`（转发，`ct status dnat`）、`forward`（UDP、ping 等，转发）、`input`（被 redirect 的 TCP，入站，`ct status dnat`）。

- **记下新连接**：`inet tunlr_gateway` 中每条路两个超时集合（设备地址 . 协议，5 分钟）：`_seen` 在 `mangle - 1`，早于其他防火墙；`_passed` 在 `filter + 200`，晚于 iptables、UFW 和 firewalld。只记录，不改变去向。
- **判断**：某设备的一类流量 5 分钟内有新连接、却一个都没进 `_passed`，即被丢弃。DNS 的 UDP 与 TCP 分开算。只认“全部被丢弃”（部分丢弃可能是有意限制），不需要阈值；超时集合让放行后自然恢复。由 `doctor` 的“Gateway traffic”报告。
- **可疑的链**：挂在 forward、input 上、策略或最后一条为无条件 drop/reject 的基础链；有无条件 `ct status dnat accept` 的不算 `dns`、`input` 的嫌疑。按表名、链名认出 UFW、firewalld、Docker 等，给出对应放行方法。firewalld 要把网卡加入 trusted 区域（`--add-forward` 只放行同一区域内的转发）。
- **开启前的提示**：`gateway on` 按同样规则列出可疑的链并警告，不阻止开启；静态看不出前面的规则是否已放行，以 `doctor` 为准。
- **边界**：只看优先级 (-151, 200) 之间的丢弃；报告最多延迟 5 分钟；拦截前已建立的长 UDP 流不会记到。

约束设计的实测事实：

- Docker 启动时 `ip_forward` 为 1 则转发策略保持 accept，为 0 则开启转发并设为 drop；重启 Docker 保留 `DOCKER-USER` 中的规则。
- UFW 入站 drop 挡住重定向的 TCP，转发 drop 挡住 DNS、UDP；firewalld 先放行 DNAT 连接，挡 UDP、ping，reload 不动 tunlr 的表。
- 启动 firewalld 会停掉 `nftables.service`（随之 `flush ruleset`）和 `tunlr-firewall.service`；Docker 的链消失（`iptables` 报 incompatible，当作没有这条链），重启 Docker 才重建，由 `_check` 补规则。
- `rp_filter` 严格模式下正常，所以不检查它。

### 8.11 若将来实现 tproxy，需要处理的 Docker 冲突

- 容器发布端口的回包会被劫持 → `ct direction reply return`
- tproxy 挂在 mangle（-150），早于 Docker 的 DNAT（-100） → `fib daddr type local return`
- 显式用 `iifname` 列出要拦截的接口（docker0、`br-*` 是否代理属于策略选择）
- 用 mihomo 的 `routing-mark` 防止回环；局域网与保留地址直接 return
- 规则放在独立的 `inet tunlr` 表中，整表删除；`_check` 检测是否被他人 flush

### 8.12 局域网设备：列表与分流

家庭网络中有的设备（游戏主机、IoT）不该走代理节点，用户要能看到谁在用这台服务器，并逐台决定走代理还是直连。

**前提：设为代理或网关的设备，流量一律经过 mihomo**，直连也由 mihomo 按规则直连；不需要经过 mihomo 的设备直接用路由器做网关。

- 走法 `proxy`（默认）或 `direct`；`direct` 由受管 mixin `tunlr:filter`（8.13）在规则最前插入 `SRC-IP-CIDR,<ip>/32,DIRECT`，经流水线发布，用户 mixin 可覆盖。网关和代理端口都适用：内核看到的来源都是设备地址。
- 按来源 IP 匹配：内核不支持 `SRC-MAC`，所以提示在路由器上保留地址；记录同时存 MAC，设备换地址时认出并提示更新。
- **不做系统层绕过**（让设备完全不经内核）：要依赖 sing-tun 的 nft 表、ip rule 位置、标记位等内部行为，内核升级后可能悄悄失效、影响整个局域网；需要的场景让设备直接用路由器做网关即可。

**设备列表**：

- 主要来源是 `inet tunlr_devices` 的 nft 动态集合：在 prerouting 的 raw 优先级（早于 TCP 重定向，看到原始目标）记下 `MAC . IPv4`，分网关与代理两类，24 小时过期。只记录。另取内核 `/connections` 的连接数、邻居表补全 MAC，名称向路由器反向解析（连接带工具的标记，不被内核劫持 DNS）。
- 表在网关或代理端口局域网访问开启时加载，都关闭时删除；开关变化只替换集合、不重建表。`_check`、`_firewall` 补缺，`_poststop` 保留，`uninstall` 删除。加载失败只记日志，不影响开关。
- 不保存历史；持久保存的只有设过走法或名称的设备。
- 走法是设备的属性，所以和列出、起名、删除记录都在 `tunlr devices` 下。

### 8.13 流量过滤

`tunlr filter` 集中设置哪些流量不按订阅的规则走。都由受管 mixin `tunlr:filter` 在规则最前插入（用户 mixin 可覆盖或删除），经流水线发布。全部默认关闭，`init` 不询问。

| 项 | 规则 | 用途 |
|---|---|---|
| `device` | `SRC-IP-CIDR,<ip>/32,DIRECT` | 指定设备直连（8.12） |
| `source` | `SRC-IP-CIDR,<网段>,DIRECT` | 来源地址段直连，如 Docker 网络、VPN 网段 |
| `quic` | `AND,((NETWORK,UDP),(DST-PORT,443)),REJECT` | 拒绝 QUIC，浏览器改用 TCP；很多节点转发 UDP 不稳定 |
| `ports` | `NOT,((DST-PORT,<端口>)),DIRECT` | 只代理常用端口，其余（BT、P2P、游戏）直连 |

- 顺序即上表：直连的设备和地址段不受其他项影响；QUIC 在端口之前，端口列表不含 443 时仍被拒绝。
- QUIC 无法只拒绝最终走代理的连接（匹配时不知道结果），直连网站也退回 TCP，影响很小。
- 默认端口沿用 ShellCrash。端口列表由工具检查：内核不查（`99999`、`9000-8000` 都能通过 `-t`）。端口以 `/` 分隔：`DST-PORT,22,80` 会把 80 当作出站名。
- 地址段主机位不为零（`172.17.0.9/16`）时拒绝：清零会让整段直连，有歧义。
- 不做的：
  - 目标地址不进 TUN：做成开关容易排除 DNS 服务器、使劫持失效（8.2）；需要时 mixin 写 `route-exclude-address` 或 `IP-CIDR` 规则。
  - 中国 IP 直连：订阅规则一般已有；系统层绕过已否决（8.12）。
  - 白名单模式：需要时用 `NOT,((SRC-IP-CIDR,…))`。
  - 针对 Docker 的适配：只做通用的 `source`，网段由用户填；透明代理下内核看到的容器来源就是容器地址。

## 9. 从 ShellCrash 借鉴的功能

纳入（均已实现）：

1. **本机模式与网关模式**：二者需要的 sysctl、防火墙设置不同，`tunlr tun gateway on/off` 切换（8.10），`init` 问用途时可一并开启（第 10 节“家庭网络中的用法”）。
2. **端口访问安全**：代理端口默认只给本机；`tunlr proxy lan on` 时 nft 表 `inet tunlr_access` 只放行从选定网卡进来、来自私有地址段的连接，内核的 `lan-allowed-ips` 另外只放行私有地址段，其他来源必须设认证，避免成为开放代理（第 6 节“端口安全”）。
3. **流量过滤**：`tunlr filter`（8.13）；不做绕过内核的系统层，目标地址不进 TUN 用 mixin。
4. **自定义保留地址段**：mixin 写 `tun.route-exclude-address`（不是受管的键）。
5. **开机联网检测与防自启死循环**：`tunlr-core.service` 的 `After=/Wants=network-online.target`、`StartLimitBurst`、`OnFailure=tunlr-rescue.service`；开机时 `_prestart` 关闭过期未确认的 TUN（8.5）。
6. **版本回退**：`tunlr core rollback`、`tunlr config rollback`。
7. **诊断与查看防火墙规则**：`doctor`、`config plan --full`。
8. **备份与还原**：`tunlr backup`（第 10 节）。

由内核能力替代：保存面板节点、规则集定时更新、NTP、订阅热更新（第 6 节）。失败通知等不做的见 3.4。

## 10. 命令设计

命令树按领域组织，检验标准是没看过文档的人能猜对大部分常用操作：

- **顶层只放领域名词和少数总览命令**，动作放在子命令里。`--help` 的分组与主菜单的分类一致。
- **不带子命令时只查看**，因此没有 `list` 子命令。查看命令的参数只改变看什么；会改系统的都是子命令，动作不靠参数变成查看或另一个动作（是 `panel reset-secret`、`core rollback`，不是 `panel --reset-secret`、`core upgrade --rollback`）。
- **几个词各司其职**：`config` 是 mihomo 的配置，`settings` 是工具自己的设置，`update` 是订阅与 geo 数据的定时更新，`upgrade` 升级 tunlr 自身，`core upgrade` 升级内核。

```
Overview
tunlr status
tunlr doctor

Setup
tunlr init
tunlr uninstall
tunlr upgrade
tunlr --version

Configuration
tunlr sub
tunlr sub add | set | remove | update | use
tunlr config
tunlr config plan | apply | rollback | edit
tunlr update
tunlr update run [sub|geo]

Network
tunlr tun
tunlr tun on | off
tunlr tun gateway on | off
tunlr proxy
tunlr proxy lan on | off
tunlr proxy env [--unset]
tunlr filter
tunlr devices

Kernel and panel
tunlr core
tunlr core start | stop | restart | logs | upgrade | rollback
tunlr panel
tunlr panel reset-secret
tunlr panel lan on | off

Maintenance
tunlr settings
tunlr settings edit
tunlr backup
tunlr backup create | restore
```

`completion` 与内部命令（`_check`、`_firewall`、`_rescue`、`_update`、`_aftercare`、`_prestart`、`_poststop`、`_tun-guard`，见 8.4、8.5）不显示在帮助中。

各命令中容易做错的决定：

- `init`：可重复执行，再次执行即修复。
  - 镜像预填建议值，清空即直连，不让用户在不知情时用上镜像；订阅名以后不能改，所以要问。
  - 用途问题见“家庭网络中的用法”。已开过透明代理、网关或局域网访问时不问（再次 init 是修复，不改用途），没有订阅时不问；网卡有公网地址时再确认。
  - 写错的网卡名在动手前拒绝；用途参数与 `--json` 同用报用法错误（后续步骤合不成一份 JSON）。
  - 用途的各步骤列在一次确认中，不再逐一确认；某一步失败时已装好的部分不撤销，退出码沿用那一步的。
- `status` 的漂移：现场合成（不跑 `mihomo -t`）与生效配置比较，区分未发布的改动和在 tunlr 之外被改过的配置；菜单每 10 秒刷新，所以按输入的大小和修改时间缓存结果。
- `uninstall`：默认保留配置、订阅、状态与二进制，之后 `init` 即可恢复；`--purge` 才删除。
- `upgrade`：新二进制先运行 `--version --json` 确认版本再 `rename`，然后由新版本执行 `init --yes` 重写 unit。不保留旧版本、没有 `rollback`：旧版本都在 release 里，tunlr 换错了不会断网，与内核不同。开发版不给 `--version` 时拒绝。不进菜单、不做定时任务，状态行提示新版本即可。
- `proxy env`：子进程改不了父 shell 的环境变量，所以输出语句由用户 `eval`。不做 `env on/off`、`shell-init`：要先在 shell 配置中装函数，且与持久开关同名而行为不同。`all_proxy` 用 `socks5h://`，大小写变量各一份（curl 不认大写的 `HTTP_PROXY`，有的工具只认大写）；没有 `mixed-port` 或需要认证时退出码 3；普通用户用 `sudo tunlr proxy env`，不为此放开 root 约束。
- `settings edit`：同 `config edit`，校验通过才保存，期间文件被改过时不覆盖。不做 `config set <key>`：设置只有两项；geo 数据用 `sub set` 的参数，因为要检查后面的 mixin 会不会压过它，编辑器做不到。
- `backup`：用于换机或重装；只改配置用 `config rollback`。
  - 不直接打包 `/etc/tunlr`、`/var/lib/tunlr`：状态中混有只对本机有意义的内容（实例 ID、已知可用内核、TUN 检测期限、网关开启前的转发设置）。带上最近一次抓取的订阅内容，新机器抓不到订阅也能发布。不加密：要么引入依赖要么自己实现，文件放在哪里工具也管不了。
  - `-o -` 写到标准输出，迁移一条命令完成：`ssh old tunlr backup create -o - | ssh new tunlr backup restore - --yes`。
  - `restore` 要求已经 `init`，格式版本比当前新时拒绝，错误信息不含订阅地址。整体替换而不合并，结果可以预料；实例 ID、内核、开关、定时任务记录保持本机的。先合成校验，再自动备份当前内容、写入、发布；全有或全无，发布失败时写回自动备份（退出码 4），与 `config edit` 不同，因为还原只做一半状态难以看明白。
  - 开关不自动打开：网卡、局域网、公网地址跟着机器，TUN 还有失联的风险；交互时让用户勾选，非交互时只列出命令。
- `sub use`：`--dry-run` 不切换，只给出启用后的 plan。

**执行前确认**：会中断代理、替换内核、删除数据或丢掉用户刚做的修改的操作，执行前确认，问题下方说明影响。确认的有：发布配置的命令（`config apply`、`config rollback`、各 `on/off` 开关，diff 之后确认）、`init`、`core stop`、`core restart`、`core upgrade`、`core rollback`、`upgrade`（先查询目标版本，确认的是哪个就装哪个）、`sub remove`、`uninstall`、放弃编辑、`panel reset-secret`。

- 不确认的：`core start`、`sub add`、`sub set`、`sub update`、不带 `--apply` 的 `sub use`、`update run`、只读与内部命令。它们只是新增、可以撤回，或由 systemd 调用、不能提问；`update run` 做的就是定时任务会做的事。
- `--yes` 跳过确认。非交互模式没有 `--yes` 时在开始工作前报用法错误（退出码 2）。不支持 `--dry-run` 的命令带它时报用法错误，不当作已确认。
- 回答否或 Esc 时退出码 0。已经做了一部分（如 `config edit` 已保存）时不发布不算取消。

**从已有条目中选择**：参数是从已有条目中挑选时，交互模式下可以省略，改在列表中选，光标停在最可能的那一项，取消不算失败。不提供 `/` 过滤：列表都不长，而过滤模式要占用 Esc。非交互时省略参数照旧报用法错误或用默认值。

**主菜单**：在终端里只输入 `tunlr` 时进入；非交互模式（包括 `--json`）仍显示帮助。

- 菜单只是子命令的入口，每一项对应一个命令，用户顺便学会命令；不嵌表单，按回车就把终端交给子命令，由子命令自己问、做、报告，因此子命令要能自己问齐答案。
- 两级，按用户要做的事和操作的对象分类，不按命令树（否则“让手机走代理”要在五个分类里找），每类不超过 7 项；分类与 `--help` 的分组一一对应。huh 的 Select 没有不可选的行，所以靠分类而不是分组标题归类。
- 需要处理的事项只列“现在就该做、做什么很明确”的（未发布的改动、内核没在运行、订阅抓取失败）；在 tunlr 之外改过的配置要用户先看 diff，新版本只在状态行提示，让用户自己挑时候升级。
- 状态行只查 systemctl、内核 API 和状态文件，不联网；刷新期间选中的操作等刷新完再执行，避免争用数据。
- 不进菜单的：`uninstall`（不该顺手按到）、`upgrade`、`proxy env`（给当前 shell 用）、`config plan`（`apply` 先显示同一份 diff）等。
- 不清屏，输出留在滚动记录里；子命令失败不退出菜单；执行中 Ctrl-C 只中断这一个操作（照常回滚）。
- 取消的提问必须像提交一样正常退出：huh 取消时发送 `tea.Interrupt`，Bubble Tea 不等读取输入的协程退出就返回，下一个提问与残留协程争抢输入而丢键（曾因此误执行“停止内核”）。所以每个表单都用 `tea.WithFilter` 把中断换成正常退出；不要用每次打开 `/dev/tty` 的办法绕开。

**输出样式**：只在标准输出是终端、没有 `NO_COLOR` 或 `TERM=dumb` 时加样式，只用 16 色；进度经 `download.WithProgress` 传入，core 不依赖界面库。其他情况（管道、内部命令、`--json`）不加样式，脚本照常解析。

**面板（`tunlr panel`）**：面板只监听 `127.0.0.1`、secret 由 tunlr 生成，所以 `tunlr panel` 一次给出打开面板的全部步骤（SSH 隧道命令与带 secret 的链接）。

- 从**生效的配置**读取 `external-controller` 和 `secret`，不读状态文件，mixin 改过时也对得上。
- 隧道地址取自 `$SSH_CONNECTION`，经别名、跳板机或 NAT 时不对，所以总附上 `LocalForward` 的写法。
- 模板写 `external-ui: ui/zashboard`，不用 `external-ui-name`：用它时 `/ui/` 是目录列表，要再点一级。
- 链接的 `label` 保持固定：zashboard 只在各项完全相同时复用后端，重复打开不多出后端。
- 不使用 zashboard 的 `disableTunMode`、`disableUpgradeCore`：不限制面板能力（第 3 节）。
- `secret` 归 `tunlr:panel`，mixin 不能设置；`reset-secret` 需要重启内核。

**面板的局域网访问（`tunlr panel lan on | off`）**：让家里其他设备直接打开面板，默认关闭。

- 开关存在状态中，由 `tunlr:panel` 写 `external-controller` 的监听地址，mixin 设置它时合成拒绝。需要重启内核，失败时回滚。面板是明文 HTTP，`lan on` 时说明 secret 在局域网中可见。
- 内核 API 不能限制来源，在公网 VPS 上监听所有地址等于开放给整个互联网，访问限制全靠 `inet tunlr_access`；内核双栈监听，所以限制也管 IPv6。

**私有地址段只定义一处**（`netfilter.PrivateIPv4`、`PrivateIPv6`）：RFC 1918、`100.64.0.0/10`、`169.254.0.0/16`、`fc00::/7`、`fe80::/10`，即“不会从公网直接到达的地址”。访问限制、`lan-allowed-ips`、网关断开时放行的目标（8.10）、设备记录（8.12）、公网地址判断和帮助文字都由它而来，免得各处放行的范围不同；测试保证模板中的 `lan-allowed-ips` 与它一致。

**家庭网络中的用法**：`init` 的用途问题对应这两种（以及只给本机用）：

| 用法 | 设备上的设置 | 工具做的事 | 覆盖范围 |
|---|---|---|---|
| 代理 | 系统代理设为 `服务器:7890` | `proxy lan on` | 遵守系统代理的应用 |
| 网关 | 网关和 DNS 设为服务器 | `tun on` + `tun gateway on`（8.10），并 `proxy lan on` | 设备的全部流量 |

网关模式下也开启代理端口：有的设备不方便改网关，两种用法可以并存。

**代理端口的局域网访问（`tunlr proxy lan on | off`）**：

- 开关存在状态中，由独占的 `tunlr:proxy` 写 `allow-lan` 和 `lan-allowed-ips`，mixin 设置这两个键时合成拒绝；要密码时在 mixin 中写 `authentication`。
- `lan-allowed-ips` 是 nft 表之外的第二层：表被清空、还没补上的几分钟里端口仍不对公网开放。回环也受它限制，所以必须含 `127.0.0.0/8` 和 `::1/128`。
- `allow-lan` 热重载即生效，不重启内核。已开启时再执行 `lan on` 不发布，只更新放行的入口；`doctor` 检查生效配置与开关是否一致。

**对其他设备开放的端口：访问限制**（`inet tunlr_access`）：开启局域网访问后内核监听所有地址，能进来的连接全由这张表决定：只放行从选定网卡进来、来自私有地址段的连接。

- 不靠内核绑定地址：只能写一个地址，满足不了局域网加 Tailscale；DHCP 地址会变、网卡可能起得比内核晚；也挡不住从别的网卡进来的包。按入口网卡（`iifname`）限制没有这些问题。
- 链挂在 `raw` 优先级，早于任何 DNAT（如 TUN 的 auto-redirect）。整张表由状态整体算出、原子替换，不做增量修改。mixin 的 `listeners` 由写它的人负责。
- 放行的入口两个开关共用，关掉开关时保留：
  - 默认取没有公网地址的网卡；VPN 只取当前 SSH 会话进来的那块（其他 VPN 可能连着机场）。只有公网网卡时默认什么都不放行。
  - 网关的局域网网卡总是放行。
  - 写了不存在的网卡时报用法错误，免得写错的名字什么都放行不了；选过、暂时不在的照旧保留。
  - 放行有公网地址的网卡前单独确认。
- 开启时先加载表再发布配置，关闭时先发布再更新表，端口对外的任何时刻都只放行这些入口；没有入口时也照样加载（只放行 `lo`），不会因少了选择而全开。
- 规则不跨重启保存，由 `_prestart` 加载、`_check`、`_firewall` 补上（8.4），`_poststop` 保留，`uninstall` 删除；两个开关都关时删表。`doctor` 检查表在不在、放行的网卡是否存在或有公网地址。
- 这张表只能多挡一层，不能放行：其他防火墙挡了端口时照样打不开。

**其他设备访问本机用的地址**：服务器常有多个地址，只给默认路由网卡的地址不够（网关选了另一块网卡时那边的设备拿到的地址是错的）。

- 列出接着真实网络的网卡（同 8.10）、点对点网卡（按 `IFF_POINTOPOINT` 判断，不按名称猜）和当前 SSH 会话进来的网卡，不列容器网桥、veth；它们也是放行入口的候选。开关开着时只给放行网卡上的地址。
- 私有地址段比家里的局域网宽：从放行网卡进来的 VPN 设备（Tailscale 用 `100.64.0.0/10`）同样放行，帮助中写明。

全局参数、非交互模式与退出码见 `CLAUDE.md`。

## 11. 为将来预留

v1 只做 CLI，但以下几点从第一版起就做好，避免以后返工：

1. 所有命令支持 `--json`，输出带版本号；退出码有明确定义
2. 配置和状态文件带 schema 版本号，结构变化时写迁移
3. 每个安装实例生成唯一 ID（将来多服务器管理时用于识别）
4. 核心逻辑放在公开的 `pkg/core`，可被其他 Go 组件复用
5. 不 import mihomo 代码，保证开源协议的选择权在自己手里

**将来的 agent**：桌面端、手机端和 Web 控制台需要远程管理服务器时，需要一个常驻的 agent 接收请求。计划作为同一二进制的子命令 `tunlr agent` 放在本仓库，由 systemd 托管、默认不开启，与 CLI 共用 `pkg/core`。届时必须单独设计身份认证、传输加密和权限控制。

**开源协议**：MIT（`LICENSE`，署名 tunlr-app）。因为不链接 mihomo 代码，不受其 GPL-3.0 约束；托管服务和控制台另行决定。

## 12. 分发

- **版本号**只有一处：git tag `vX.Y.Z`，goreleaser 经 `-ldflags -X` 注入；本地 `go build` 的版本是 `dev`。
- **发布**：推送 tag 触发 `release.yml`，测试后由 goreleaser 发布 linux amd64、arm64 的压缩包和 `checksums.txt`，说明从 commit 生成。资产的 sha256 以 release API 的 `digest` 为准，`tunlr upgrade`、`core upgrade`、`install.sh` 只认它（第 7 节）。
- **安装入口**：`install.sh`（POSIX sh）经 jsDelivr 分发（中国可达），备用 raw.githubusercontent.com。API 直连查 release，资产经 `TUNLR_MIRROR`（默认 gh-proxy，设为空即直连）下载，核对 sha256、确认能运行后原子替换，再运行 `tunlr init` 并转交其余参数。
  - 默认经 gh-proxy：中国的服务器直连 GitHub 下载常常失败，字节经哪里都按官方 sha256 校验，对其他地区只是多一跳。jsDelivr 不能分发 release 资产，所以脚本走 jsDelivr、资产走镜像。
  - 有终端时由 `init` 自己问镜像（除非设了 `TUNLR_MIRROR`），没有控制终端时（要试着打开 `/dev/tty` 才知道）传 `--mirror` 和 `--yes`，绝不阻塞。
  - `uninstall.sh` 在 `tunlr` 不在时先装一份再执行 `uninstall --purge`。
- 可审计：`tunlr config plan --full` 输出将写入系统的全部内容。

## 13. MVP 顺序

1. 配置流水线：模板 + mixin → 校验 → 原子写入 → 重载 → 失败回滚
2. systemd 托管与内核安装、升级、已知可用版本
3. 透明代理开关（TUN + auto-redirect），带自动检测与超时回滚
4. `doctor` 诊断
5. 本机/网关模式、端口安全（第 9 节 1、2）

以上和之后排期的功能（主菜单、执行前确认、定时任务、面板、受管 mixin、局域网访问与 `init` 的用途问题、`status` 的漂移、局域网设备、流量过滤、代理变量、工具设置、备份与还原、命令树整理、mixin 命名空间、`tunlr upgrade`、发布）均已完成，v0.1.0 已发布。

### 13.1 待实现（按顺序）

动手前先讨论用户可见的行为，补全设计再实现。目前没有排定的项目。

- 不做或暂不做：失败通知（3.4）；局域网设备绕过内核的系统层（8.12）。`completion` 由 cobra 自动提供。
- 草案，位置待定：常用网站的连通性测试（暂名 `tunlr probe`）。经代理端口按真实分流规则请求一组常用网站（国内、海外、AI、流媒体各几个，可在设置中替换），报告是否连通、耗时，并经 `/connections` 按源端口找到这条连接，给出匹配的规则和链路；只在用户执行时出网，不在 `status` 和菜单刷新中进行。它代替“在 status 中显示出口 IP 或当前节点”：分流下各网站的出口不同，单个 IP 或节点会误导。

## 14. 待决事项

- 商标查询
- 官网（`tunlr.app`）与 `tunlr.app/install.sh` 入口
