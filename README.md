# tunlr

tunlr（读作 “TUN-ler”，tun + -lr）是在 Linux 服务器上运行 [mihomo](https://github.com/MetaCubeX/mihomo) 的运维工具：一个静态二进制，负责内核的安装与升级、systemd 托管、订阅与配置的合成、校验、发布与回滚、透明代理和网关模式、局域网设备的接入与分流、环境诊断。切节点、测速、看连接和日志等运行时交互交给 [zashboard](https://github.com/Zephyruso/zashboard) 面板。

> 本项目与 MetaCubeX 无关，仅在描述用途时提及 mihomo。

## 定位

- **mihomo 的运维层**：工具只管磁盘上的文件和系统状态；常驻的只有 mihomo 内核，定时任务、救援、回滚全部交给 systemd，tunlr 自己不常驻。
- **每个改动都是一次事务**：准备 → 校验（`mihomo -t`）→ 应用 → 确认，失败则回滚。配置用临时文件加 `rename` 原子替换，保留最近 10 个修订；内核升级前先用新二进制校验当前配置，保留已知可用的旧版本，启动失败时自动换回。
- **开启透明代理不会失联**：开启后自动检测 DNS、直连、代理和 SSH，检测不过或超时即回滚；网关模式同样。
- **不做面板已有的功能**：不切节点、不测速、不跟日志，只做 Linux 侧必须由工具负责的事。
- **不做遥测、账号和云同步**。

## 支持范围

- 使用 systemd 的 Linux（Debian 12+、Ubuntu 22.04+ 等）
- amd64、arm64
- nftables（网关模式在 Docker 的 `DOCKER-USER` 链中插入放行规则时例外地使用 `iptables`）
- 以 root 运行

不支持 OpenRC、procd、路由器固件、WSL、iptables 防火墙、sing-box 等其他内核。环境不满足时 `tunlr doctor` 明确拒绝，不做兜底兼容。

## 安装

```bash
curl -fsSL https://cdn.jsdelivr.net/gh/tunlr-app/cli@main/install.sh | sudo sh
```

jsDelivr 不可用时改用 GitHub 的原始文件：

```bash
curl -fsSL https://raw.githubusercontent.com/tunlr-app/cli/main/install.sh | sudo sh
```

脚本做的事：检查 root、systemd 和架构 → 向 `api.github.com` 查询最新发布 → 经镜像下载 `tunlr_<版本>_linux_<架构>.tar.gz` → 核对 GitHub 为该文件记录的 sha256 → 确认下载的二进制能运行 → 原子替换到 `/usr/local/bin/tunlr` → 运行 `tunlr init`。`init` 会询问 GitHub 镜像（预填 `https://axisnow.gh-proxy.org/`，清空即直连）、订阅地址、名称（预填 `default`，之后不能改）及节点筛选、这台服务器的用途（只给本机用、给局域网设备当代理、当网关），都可以留空或之后再设。

环境变量和参数：

| 变量 | 作用 |
|---|---|
| `TUNLR_MIRROR` | 脚本下载 tunlr 用的 GitHub 镜像前缀，默认 `https://axisnow.gh-proxy.org/`；设为空（`sudo env TUNLR_MIRROR= sh`）则直连 github.com。镜像只传字节，sha256 取自 GitHub 的 API，不一致即拒绝。有终端时 `init` 会问之后用哪个镜像（输入框预填这个默认值，清空即直连）；没有终端或设了这个变量时直接作为 `--mirror` 传给 `init`，之后内核和 geo 数据的下载也经它 |
| `TUNLR_VERSION` | 安装指定版本，如 `v0.2.0`；默认最新发布 |
| `TUNLR_INIT=no` | 只安装二进制，不运行 `tunlr init` |

脚本之后的参数原样传给 `tunlr init`，例如：

```bash
curl -fsSL https://cdn.jsdelivr.net/gh/tunlr-app/cli@main/install.sh | sudo sh -s -- --sub home=https://example.com/sub --gateway auto
```

没有终端时（自动化部署）`init` 以 `--yes` 运行，不提问。也可以手动下载 [release](https://github.com/tunlr-app/cli/releases) 中的压缩包，解出 `tunlr` 放到 `/usr/local/bin/`，再运行 `tunlr init`。

## 快速开始

```bash
tunlr            # 在终端里直接运行进入主菜单：状态、分类、等价命令都在上面
tunlr status     # 服务、内核、订阅、配置修订、透明代理、局域网访问、可用的更新
tunlr doctor     # 环境诊断：systemd、nftables、TUN、网络、内核与配置的一致性
tunlr panel      # 打开面板的方式：SSH 隧道命令和链接
```

主菜单是终端里的两级列表（不占用整个屏幕，所有输出都留在滚动记录里），每一项都显示等价的命令行，菜单里能做的事命令行都能做，`--json` 和非交互模式不受影响。

常见的下一步：

```bash
tunlr sub add home https://example.com/sub     # 添加订阅（可保存多个，同一时间启用一个）
tunlr config plan                               # 预览将发布的配置（diff）
tunlr config apply                              # 发布：mihomo -t 校验通过后热重载，失败则回滚
tunlr tun on                                    # 本机流量经 mihomo（带检测与回滚）
tunlr tun gateway on                            # 局域网设备把本机当网关
tunlr proxy lan on                              # 局域网设备把本机当 HTTP/SOCKS5 代理
tunlr panel lan on                              # 局域网设备直接打开面板
```

订阅之外的自定义写在 `/etc/tunlr/mixin.d/*.yaml`（`tunlr config edit`），分 `mihomo:`（原样合并进配置）和 `tunlr:`（对节点、策略组、规则做前插、追加、修改、删除）两个命名空间；订阅更新后会按模板、订阅、mixin 重新合成并发布。TUN 的堆栈（默认 `mixed`，可改 `system` 或 `gvisor`）等 tunlr 不管理的 `tun` 键也在这里改，例如 `tunlr config edit 20-tun` 写入 `mihomo: {tun: {stack: system}}`。顶层的 `ipv6` 由 `tunlr ipv6` 决定，mixin 写了会拒绝发布（见下文“IPv6”）。

## 命令总览

顶层只放领域名词和总览命令，不带子命令时只查看、不改动。全局参数：`--json`、`--yes`、`--dry-run`、`--verbose`；标准输入不是终端时进入非交互模式，所有问答都必须由参数给出。退出码：`0` 成功，`1` 一般错误，`2` 用法错误，`3` 检查未通过，`4` 失败但已回滚。

```
Overview
  status                      当前状态
  doctor                      环境诊断（--offline 跳过网络检查）

Setup
  init                        首次安装向导；再次执行即修复
  upgrade                     升级 tunlr 自身到最新发布（--version 指定版本）
  uninstall                   卸载并清理规则（--purge 一并删除配置、订阅与状态）

Configuration
  sub                         已保存的订阅；add、set、remove、update、use
  config                      当前修订、历史与 mixin；plan、apply、rollback、edit
  update                      定时更新的日程（订阅、geo 数据）；update run [sub|geo] 立即执行

Network
  tun                         透明代理与网关模式；tun on|off、tun gateway on|off
  proxy                       代理端口的地址；proxy lan on|off、proxy env
  dns                         mihomo 问谁、怎么回答 DNS 查询；dns servers、dns mode、dns ipv6、dns fake-ip-filter
  ipv6                        连 IPv6 地址的连接经不经 mihomo；ipv6 rules|bypass
  filter                      流量过滤：source 网段直连、quic 拒绝 QUIC、ports 只代理常用端口
  devices                     使用本机的局域网设备，终端里选中一台再起名、设直连或忘记；name、route、forget、--all

Kernel and panel
  core                        内核与服务；start、stop、restart、logs、upgrade、rollback
  panel                       面板的打开方式；panel lan on|off、panel reset-secret

Maintenance
  settings                    工具设置（settings.yaml）；settings edit
  backup                      备份与还原；backup create、backup restore
```

每个命令都有 `--help`；完整的行为说明见 [docs/DESIGN.md](docs/DESIGN.md) 第 10 节。

## 定时任务与升级

- `tunlr-update.timer` 每小时唤醒一次，到期的订阅重新抓取，启用的订阅抓取成功后重新发布配置；geo 数据由内核按配置自己更新。周期按订阅设置（`tunlr sub set <名称> --update 1d`），`tunlr update` 查看日程。
- 同一个定时任务每天查询一次 tunlr 和 mihomo 的最新版本（只查询，不下载，不自动升级）。有新版本时主菜单的状态行和 `tunlr status` 提示，升级由用户执行；tunlr 自己的升级也在主菜单的 Maintenance 里，有新版本时第一级最上面也有一项，升级后菜单用新版本重新打开：

```bash
tunlr upgrade          # 升级 tunlr 自身：下载、校验 sha256、确认能运行、原子替换，然后由新版本重写 unit、重载 nft 规则；内核的 unit 没变时 mihomo 不重启
tunlr core upgrade     # 升级 mihomo：下载、校验、用新内核校验当前配置、重启并做健康检查，失败则换回
tunlr core rollback    # 换回上一个已知可用的内核版本
```

- 下载慢或断开时自动续传、重试，不限总时长；只有连续 5 轮收不到数据才放弃。
- 在别处下载好官方的内核资产（文件名要保持原样，如 `mihomo-linux-amd64-v3-v1.19.33.gz`）再安装：`tunlr core upgrade --file <文件>`，首次安装用 `tunlr init --kernel-file <文件>`。只向 GitHub 查询该版本的发布信息来核对 sha256，不下载资产。

- 面板里升级内核也可以：`tunlr-aftercare.path` 在内核文件变化后核对它，启动失败时 `tunlr-rescue.service` 换回已知可用版本。

## 卸载

```bash
tunlr uninstall            # 停止 mihomo，删除 unit、内核、规则和用户；保留设置、订阅、历史
tunlr uninstall --purge    # 连同 /etc/tunlr、/var/lib/tunlr 和 /usr/local/bin/tunlr 一起删除
```

或者：

```bash
curl -fsSL https://cdn.jsdelivr.net/gh/tunlr-app/cli@main/uninstall.sh | sudo sh
```

## GitHub 镜像

中国的服务器直连 GitHub 下载常常失败。tunlr 的下载（内核、tunlr 自身）都可以经前缀式镜像传输：`tunlr init --mirror https://axisnow.gh-proxy.org/`，或 `tunlr settings edit` 修改 `github-mirror`。无论经哪个镜像，sha256 都只取自 GitHub 的 release API，不一致即丢弃；API 本身不经镜像。配置中 `external-ui-url`、`geox-url` 的 GitHub 地址也会改为经镜像，由内核自己下载。tunlr 兜底的 geo 数据地址本来就不走 GitHub：用的是 meta-rules-dat 的 release 分支经 jsDelivr 的 fastly 节点（`https://fastly.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@release/…`），各节点缓存 12 小时，可能比上游晚一天。镜像的域名不加直连规则，和其他流量一样按订阅规则分流；想让它直连，在 mixin 里加 `tunlr.rules.prepend: [DOMAIN,axisnow.gh-proxy.org,DIRECT]`。只支持“镜像前缀 + 完整 GitHub 地址”这一种写法，jsDelivr 这类按仓库路径改写的 CDN 不能用作 `github-mirror`，但可以按文件给 geo 数据指定来源：`tunlr sub set <名称> --geo-url geosite=https://cdn.jsdelivr.net/...`。

## DNS 怎么走

默认用 tunlr 的 DNS 和嗅探，订阅写了 `dns`、`sniffer` 也不用（`hosts` 照常采用）：机场的 DNS 是为桌面客户端写的，在服务器上常常不合适。tunlr 的兜底值：

- fake-ip 模式，假地址取自 `198.18.0.1/16`；不返回 IPv6 地址（`tunlr dns ipv6 on` 可以打开），程序只经 IPv4 连接。
- `geosite:cn` 的域名经直连问阿里、腾讯的 DoH。其他域名问 Cloudflare、Google 的 DoH，按订阅的规则连接（`respect-rules`），通常经代理：查询不经过中国的解析器，`GEOIP,CN` 这类规则也不会因被污染的地址判错。节点的域名经直连用阿里、腾讯解析。节点都不可用时境外的域名解析不出来，但这时连接本来也不通。上游全是加密的 DoH（`default-nameserver` 也是），运营商看不到、改不了问了什么。
- `+.lan`、`+.home.arpa` 的名字问本机的解析设置（`system`，通常是路由器），局域网设备的名字照样解析得出。
- 局域网、时间同步、联网检测、STUN、QQ 快捷登录、游戏主机等域名返回真实地址（`fake-ip-filter`）。
- 嗅探 HTTP、TLS、QUIC：局域网设备自己用 DoH 或写死了 DNS 时，按域名的规则照样生效。

`tunlr dns` 显示 DNS 模式、IPv6 的设置、fake-ip 的地址段、问谁、你加的 fake-ip-filter 条目，以及各来自哪里；在终端里接着选改哪项，选了模式再选取值，选了 IPv6 再分两步选（主菜单 Traffic 中的 DNS 就是它）。问谁在 TUN 关着时也用到：代理端口来的连接、节点的域名都由 mihomo 自己解析；模式和 fake-ip-filter 只在开了 TUN 时用到。这些命令改的都是 mixin 文件：现在决定那个键的文件，没有时写进 `mixin.d/90-dns.yaml`；预览列出要改的文件和键，确认后发布。自己编辑这些文件效果相同，命令下次读到的就是你改的。

- `tunlr dns mode fake-ip|redir-host`：DNS 模式（`dns.enhanced-mode`）；`tunlr dns mode default` 从共用的和启用的订阅的 mixin 中删除它，回到按配置；不给模式时在终端里选。fake-ip 回答假地址（取自 `dns.fake-ip-range`，tunlr 的兜底值是 `198.18.0.1/16`，`tunlr dns` 显示生效的），经代理的域名不必真的解析；有些应用、设备不接受假地址，可以换 redir-host，回答真实地址，按域名的规则靠映射和嗅探照样生效。DNS 模式只影响 mihomo 自己回答的查询：开了 TUN 时本机的，和把这台服务器当网关、DNS 的局域网设备的。
- `tunlr dns servers`：问谁，分两组。中国的域名（`geosite:cn`）经直连问，也用来解析节点的域名，写进 `dns.nameserver-policy` 的 `geosite:cn`（加 `#DIRECT`）和 `dns.proxy-server-nameserver`；其他域名按规则问，写进 `dns.nameserver`。在终端里打开两个文本框，预填现在用的服务器（没有时填 tunlr 的默认值），一行一个，IP、`udp://`、`tcp://`、`tls://`（DoT）、`https://`（DoH）、`quic://`（DoQ）、`h3://`、`dhcp://<网卡>`、`system` 可以混写；清空一个框就删除你的设置。发布后经 mihomo 解析 `www.baidu.com` 和 `www.google.com`，不通时问你是否放回原来的服务器。没有终端时用 `--cn`、`--other`（可重复，替换整组）或 `--reset`。
- `tunlr dns fake-ip-filter`：fake-ip 模式下哪些域名返回真实地址。在终端里打开文本框，只编辑你自己加的条目，一行一条（`+.lan`、`*.example.com`、`nas.example.com`）：Enter 换行，Tab 完成，Ctrl+E 用编辑器打开，Esc 取消。没有终端时用 `--add`、`--remove`、`--reset`。条目写在 `90-dns.yaml` 的 `tunlr.fake-ip-filter.append`，加在生效的那份列表上，那份列表以后变了照样生效；要去掉那份列表里的条目，在 mixin 中写 `delete`（可以用 `/正则/`）。
- `tunlr sub set <订阅> --dns sub`：这个订阅启用时采用它写的 `dns` 和 `sniffer`（它没写的仍用 tunlr 的），`--dns tunlr` 恢复默认。它写的是订阅自己的 mixin `mixin.d/<订阅>/20-dns.yaml`：

  ```yaml
  tunlr:
    sub-dns: true
  ```

  `sub-dns` 也可以写在共用的 mixin 里，对所有订阅生效；按文件顺序最后写的为准，`false` 取消前面的。用订阅的 DNS 时 `tunlr status` 多一行 `DNS:`；这时切到 redir-host，订阅的 DNS 缺 `respect-rules` 或 `nameserver-policy` 会提醒。订阅的 DNS 以明文直连问上游时，预览和 `status` 会提醒（见下文），但不替你改写。

### IPv6

IPv6 分两层设置，在开着 TUN 时起作用，这时本机的程序经 mihomo 解析域名、连接也经过它：

| 层 | 管什么 | 命令 | 存在哪 |
|---|---|---|---|
| 流量 | 连 IPv6 地址的连接经不经 mihomo（顶层 `ipv6`） | `tunlr ipv6 rules\|bypass` | tunlr 的状态 |
| 回答 | 域名解不解析出 IPv6 地址（`dns.ipv6`） | `tunlr dns ipv6 on\|off\|default` | mixin |

`tunlr ipv6` 显示两层，在终端里接着分两步选：先选流量，选了 rules 再选回答，在第二步按 Esc 回到第一步；两层一起改时只确认、发布一次（主菜单 DNS 中的 IPv6 也是这样）。`tunlr dns ipv6` 不给取值时只选回答。两层的取值不能混用，`tunlr ipv6 on` 会提示改用 `tunlr dns ipv6 on`。

流量：

- `rules`（默认）：mihomo 接管 IPv6，连 IPv6 地址的连接进 TUN、经 mihomo 按规则走；写死在程序里的 IPv6 DNS 服务器和 IPv6 地址也一样，本机以后有了 IPv6 也不会绕过代理。
- `bypass`：mihomo 不接管 IPv6（顶层 `ipv6: false`），自己也不用 IPv6。域名仍只解析出 IPv4 地址：mihomo 不回答 IPv6 地址（回答设了 on 也一样），本机发往 IPv6 DNS 服务器的查询也被拒绝（`inet tunlr` 中的一条规则），改问 IPv4 的服务器、由 mihomo 回答。所以查域名的程序照样经 mihomo、走 IPv4，只有直接连 IPv6 地址的连接按系统路由直连，不经代理、也不按规则。只配了 IPv6 DNS 服务器、因此解析不了时，tunlr 改回原来的设置（退出码 4）；在 TUN 关着时设了 bypass，之后 `tun on` 的 dns 检测会因此失败并说明原因。本机没有 IPv6 出口时看不出区别，有了 IPv6 之后 IPv6 的连接就不经代理。开着时 `tunlr status` 多一行 `IPv6: bypass`。

回答：

- `off`：tunlr 的兜底值（`dns.ipv6: false`）。程序查域名只拿到 IPv4 地址，只经 IPv4 连接。
- `on`：`dns.ipv6: true`。fake-ip 下域名拿到 `dns.fake-ip-range6` 中的假 IPv6 地址（配置中没有时写 `fdfe:dcba:9876::1/64`），只有 IPv6 的域名也能访问，本机没有 IPv6 也行；redir-host 下拿到真实地址，直连它们要本机有 IPv6 出口（嗅探出域名的除外），经代理的要节点能访问 IPv6。网关只转发 IPv4：网关开着时打开它，经路由器有 IPv6 的局域网设备会把 IPv6 的连接发给路由器（fake-ip 下连不上、等一会儿才改用 IPv4，redir-host 下绕过 mihomo），命令、`tunlr dns`、`tun gateway on` 和 doctor 的 `gateway-ipv6` 都会提醒。
- `default`：回到按配置（兜底值 off，或 `--dns sub` 时订阅写的）。

bypass 时回答不起作用，照样可以改：写进 mixin，换回 rules 后生效，`tunlr ipv6` 在回答一行注明 `not used while IPv6 bypasses mihomo`。

不想让 IPv6 绕过代理（包括本机现在没有 IPv6、以后可能有的情况）：流量 `rules`，回答 `off`，也就是默认值。用了订阅的 DNS（`--dns sub`）时，明确写 `tunlr dns ipv6 off`，免得订阅的 `dns.ipv6: true` 生效。

TUN 关着时本机的程序不经 mihomo，这些设置只影响 mihomo 自己发出的连接（代理端口来的、连节点的）。

回答写 `dns.ipv6`（off 同时删去 mixin 中的 `fake-ip-range6`，default 删去两者），写在现在决定它的 mixin，没有时写进 `90-dns.yaml`。流量存在 tunlr 的状态中，顶层的 `ipv6` 由它决定（受管 mixin `tunlr:ipv6`），mixin 写了会拒绝发布。

### DNS 泄露

DNS 泄露指查询绕过 mihomo、或以明文直连问上游：运营商能看到、改写你问了哪些域名，按域名的规则也可能因此判错。`tunlr status` 有一行 `DNS leaks:`，`--json` 中是 `dns_leaks`；`tunlr doctor` 的 install 组有对应的几项（都是 warn）。不联网，只看配置、路由和网关记下的流量：

- `dns-servers`：生效的配置以明文直连问上游。明文是不写协议的 IP、`udp://`、`tcp://`、`dhcp://` 和 `system`；带 `#节点名` 或开了 `respect-rules` 的算经代理（`#DIRECT`、`system`、`dhcp://` 总是直连）。只看 `nameserver`、`fallback` 和 `nameserver-policy`：只含中国、局域网域名的 policy 项（`geosite:cn`、`*-cn`、`geosite:private`、`+.cn`、`+.lan` 等），以及只解析节点、直连域名的 `proxy-server-nameserver`、`default-nameserver`、`direct-nameserver` 不算。`dns.enable: false` 也算：mihomo 经系统的解析设置直连询问。`respect-rules` 下规则仍可能让 DNS 直连，这里不解析规则。
- `dns-tun-ipv6`：TUN 开着，本机访问 IPv6 公网却不经 `tunlr0`，经 IPv6 的查询和连接都绕过 mihomo。tunlr 的 unit 让 mihomo 不论启动时有没有 IPv6 都给 TUN 配 IPv6 地址（`SKIP_SYSTEM_IPV6_CHECK=1`）；unit 是旧的时先 `tunlr init`，再 `tunlr core restart`。`tunlr ipv6 bypass` 时 IPv6 按设置不经 `tunlr0`，不算泄露，只在拒绝 IPv6 DNS 查询的规则不在时警告。
- `dns-gateway-ipv6`：网关开着，局域网有 IPv6。网关只转发 IPv4，路由器经 IPv6 发给设备的 DNS 服务器（RDNSS、DHCPv6）和路由都绕过这台服务器；关掉路由器的 IPv6，至少关掉它发的 IPv6 DNS。
- `dns-gateway-devices`：最近 5 分钟经网关上网、却没有经它问 DNS 的设备，多半直接问路由器。设备的 DNS 要设成这台服务器，不能是路由器；用 DoH、DoT 的设备、这段时间里只用了缓存的设备也会列出，所以只说可能。

代理端口来的设备、TUN 关着时本机程序自己的查询不在检查之内。

`tunlr config plan` 和 `tunlr config` 有一行 `DNS settings:`，说明 dns、sniffer 来自订阅还是兜底值、被哪些 mixin 改过；`tunlr config` 还列出兜底值的完整内容。命令没有覆盖的键直接写 mixin（`tunlr config edit 10-dns`），映射逐层合并，只写要改的键：

```yaml
mihomo:
  sniffer:
    enable: false   # 不嗅探
```

列表在 `mihomo:` 下整体替换；`fake-ip-filter` 用 `tunlr.fake-ip-filter` 加减条目，不必整份复制。写在 `mihomo:` 下改的是生效的那一份（订阅的或兜底的）；只想改兜底值、订阅的 DNS 被采用时仍用订阅的，写在 `tunlr: {defaults: ...}` 下。

## 磁盘布局

| 路径 | 内容 |
|---|---|
| `/usr/local/bin/tunlr` | 工具本身 |
| `/etc/tunlr/` | `settings.yaml`、`subscriptions.yaml`（含机场凭据，0600）、`mixin.d/` |
| `/var/lib/tunlr/` | 内核（`bin/`、`kernels/`）、内核的工作目录（`core/`）、订阅内容、配置历史、自动备份、`state.json` |
| `/etc/systemd/system/tunlr-*` | `tunlr-core.service` 及定时、救援、善后等 unit |

`tunlr config plan --full` 输出将写入系统的全部内容（mihomo 配置、nft 脚本、systemd unit），执行前可以审查。

## 开发

```bash
go test ./...
```

```bash
goreleaser build --snapshot --clean
```

本地 `go build` 的版本是 `dev`，`tunlr upgrade` 要求指明 `--version`。发布：推送 `vX.Y.Z` tag，GitHub Actions 运行测试并用 goreleaser 发布 linux amd64、arm64 的压缩包和 `checksums.txt`。nftables 相关的集成测试在 network namespace 中进行（`TUNLR_TEST_NETNS=1`），需要 root 的 Linux 环境。

设计决策和取舍见 [docs/DESIGN.md](docs/DESIGN.md)，给编码助手的约定见 [CLAUDE.md](CLAUDE.md)。

## 协议

[MIT](LICENSE)。tunlr 不 import mihomo 的任何代码，只经 REST API 与独立运行的内核交互。
