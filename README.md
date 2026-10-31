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

脚本做的事：检查 root、systemd 和架构 → 向 `api.github.com` 查询最新发布 → 经镜像下载 `tunlr_<版本>_linux_<架构>.tar.gz` → 核对 GitHub 为该文件记录的 sha256 → 确认下载的二进制能运行 → 原子替换到 `/usr/local/bin/tunlr` → 运行 `tunlr init`。`init` 会询问 GitHub 镜像（预填 `https://gh-proxy.com/`，清空即直连）、订阅地址及节点筛选、这台服务器的用途（只给本机用、给局域网设备当代理、当网关），都可以留空或之后再设。

环境变量和参数：

| 变量 | 作用 |
|---|---|
| `TUNLR_MIRROR` | 脚本下载 tunlr 用的 GitHub 镜像前缀，默认 `https://gh-proxy.com/`；设为空（`sudo env TUNLR_MIRROR= sh`）则直连 github.com。镜像只传字节，sha256 取自 GitHub 的 API，不一致即拒绝。有终端时 `init` 会问之后用哪个镜像（输入框预填这个默认值，清空即直连）；没有终端或设了这个变量时直接作为 `--mirror` 传给 `init`，之后内核和 geo 数据的下载也经它 |
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

订阅之外的自定义写在 `/etc/tunlr/mixin.d/*.yaml`（`tunlr config edit`），分 `mihomo:`（原样合并进配置）和 `tunlr:`（对节点、策略组、规则做前插、追加、修改、删除）两个命名空间；订阅更新后会按模板、订阅、mixin 重新合成并发布。TUN 的堆栈（默认 `mixed`，可改 `system` 或 `gvisor`）等 tunlr 不管理的 `tun` 键也在这里改，例如 `tunlr config edit 20-tun` 写入 `mihomo: {tun: {stack: system}}`。

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
  filter                      流量过滤：source 网段直连、quic 拒绝 QUIC、ports 只代理常用端口
  devices                     使用本机的局域网设备；name、route、forget

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
- 同一个定时任务每天查询一次 tunlr 和 mihomo 的最新版本（只查询，不下载，不自动升级）。有新版本时主菜单的状态行和 `tunlr status` 提示，升级由用户执行：

```bash
tunlr upgrade          # 升级 tunlr 自身：下载、校验 sha256、确认能运行、原子替换，然后由新版本重写 unit
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

中国的服务器直连 GitHub 下载常常失败。tunlr 的下载（内核、tunlr 自身）都可以经前缀式镜像传输：`tunlr init --mirror https://gh-proxy.com/`，或 `tunlr settings edit` 修改 `github-mirror`。无论经哪个镜像，sha256 都只取自 GitHub 的 release API，不一致即丢弃；API 本身不经镜像。配置中 `external-ui-url`、`geox-url` 的 GitHub 地址也会改为经镜像，由内核自己下载。镜像的域名不加直连规则，和其他流量一样按订阅规则分流；想让它直连，在 mixin 里加 `tunlr.rules.prepend: [DOMAIN,gh-proxy.com,DIRECT]`。只支持“镜像前缀 + 完整 GitHub 地址”这一种写法，jsDelivr 这类按仓库路径改写的 CDN 不能用作 `github-mirror`，但可以按文件给 geo 数据指定来源：`tunlr sub set <名称> --geo-url geosite=https://cdn.jsdelivr.net/...`。

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
