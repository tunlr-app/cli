# CLAUDE.md

本文件给 Claude Code 等编码助手阅读，记录本仓库每次工作都必须遵守的约定。完整的设计决策和取舍理由见 `docs/DESIGN.md`。

## 项目是什么

- **tunlr CLI**：在 Linux 服务器上运行 mihomo 的运维工具。
- 品牌 tunlr，GitHub 组织 `tunlr-app`，本仓库 `tunlr-app/cli`，命令名 `tunlr`，官网 `tunlr.app`。
- 定位：**mihomo 的运维层**。工具负责“磁盘上的文件”和“系统状态”，运行时交互（切节点、测速、连接、日志、流量）全部交给 zashboard。
- 本项目与 MetaCubeX 无关。mihomo 上游要求无关的下游项目名称中不得包含 “mihomo”，所以仓库名、二进制名、包名、systemd unit 名都不能出现这个词；只能在描述用途时提及。

## 硬性约束

- 语言：Go。产物是一个 `CGO_ENABLED=0` 的静态二进制，模板用 `go:embed` 打包。
- **不要 import mihomo 的任何 Go 包**。只通过 REST API 与独立运行的 mihomo 进程交互，配置结构只定义自己需要的字段。这保证本项目不受 mihomo 的 GPL-3.0 约束。
- **不做全屏 TUI**。交互只用 `huh`（问答表单）和 `lipgloss`（美化输出），不直接写 Bubble Tea 的 Model/Update/View。
- **不做常驻守护进程**。常驻的只有 mihomo 内核；定时任务、救援、回滚全部交给 systemd。
- 只支持：使用 systemd 的 Linux（Debian 12+、Ubuntu 22.04+ 等）、amd64 与 arm64、nftables、root 运行工具。不支持 OpenRC/procd/路由器固件/WSL、iptables、sing-box 及其他内核。环境不满足时由 `doctor` 明确拒绝，不做兜底兼容。
- 不做 zashboard 已有的功能。例外：Linux 侧必须由工具负责的事项（如 TUN 的持久化开关）。
- 不限制面板能力：内核支持的操作（如面板触发内核升级）保持可用，工具负责兜底救援。
- 不做遥测、账号体系、云同步。

## 依赖

| 用途 | 选型 |
|---|---|
| CLI | `spf13/cobra` |
| 交互与输出 | `huh`、`lipgloss` |
| YAML | `go.yaml.in/yaml/v3`（不要用已归档的 `gopkg.in/yaml.v3`），基于 `yaml.Node` 合并以保留键顺序与注释 |
| diff | `sergi/go-diff` |
| 内核 API | 标准库 `net/http`，经 Unix socket（`external-controller-unix`）连接 |
| 实时数据流 | `coder/websocket` |
| 并发互斥 | `gofrs/flock` |
| nftables | `text/template` 生成脚本，`nft -f` 原子加载；仅在需要程序化查询时用 `google/nftables` |
| systemd | 直接执行 `systemctl` |
| 日志 | `log/slog` |
| 发布 | goreleaser（amd64、arm64，带 checksum） |

新增依赖前先确认标准库或现有依赖不能满足。

## 目录结构

```
cmd/tunlr/          main 入口（编译出的二进制名为 tunlr）
pkg/core/           全部业务逻辑，公开包，将来供 agent 等组件复用
  config/           模板 + 默认值 + 订阅 + mixin + 强制项合成、diff、发布历史
  pipeline/         配置流水线：预览（plan）、校验、发布（apply）、回滚
  subscription/     订阅的抓取、保存、增删与切换（可保存多个，同一时间只启用一个）
  mihomo/           内核下载、升级、已知可用版本、geo 数据、mihomo -t 校验、API client
  netfilter/        nft 规则生成、应用、清理；TUN 前置检查
  service/          systemd unit 与 timer 生成和控制
  doctor/           环境诊断
  lock/             文件锁
  paths/            磁盘布局（所有路径都从这里取，测试时用 Root 指向临时目录）
  fsutil/           原子写入等文件操作
  state/            状态文件、安装实例 ID、结构版本迁移
  settings/         工具设置（/etc/tunlr/settings.yaml）
  download/         GitHub release 下载：可经镜像传输，只认官方 API 的 sha256
internal/cli/       cobra 命令，只做参数解析、调用 core、渲染输出
templates/          nft、systemd、默认配置模板
docs/               设计文档
```

规则：任何逻辑都放在 `pkg/core`，`internal/cli` 只是薄壳。如果某个操作只能在 CLI 层完成，说明逻辑放错了地方。

## 命令约定

命令树见 `docs/DESIGN.md` 的“命令设计”。实现时遵守：

- 全局参数：`--json`、`--yes`、`--dry-run`、`--verbose`。
- 每个命令都支持 `--json`，输出结构带 `version` 字段，供将来的其他组件调用。
- 标准输入不是终端时进入非交互模式：所有问答都必须能通过参数给出答案，否则报错退出，绝不阻塞等待输入。
- 退出码：`0` 成功；`1` 一般错误；`2` 参数用法错误；`3` 检查未通过；`4` 操作失败但已自动回滚。
- systemd 调用的内部命令以下划线开头（`_check`、`_rescue`、`_update`），不出现在帮助中。
- 工具自己生成的 systemd unit 统一使用 `tunlr-` 前缀，例如 `tunlr-core.service`、`tunlr-update.timer`。

## 可靠性原则

- 每个会修改系统的操作都按“准备 → 校验 → 应用 → 确认，失败则回滚”的事务模式实现。
- 配置发布前必须用 `mihomo -t` 校验；写入用临时文件加 `os.Rename` 原子替换；保留最近若干份历史。
- 内核升级前先用新二进制对当前配置跑 `-t`，通过后再替换，并保留已知可用的旧版本。
- 所有会改动系统的命令执行前先获取文件锁，避免与 timer 触发的任务并发。
- 配置和状态文件从第一版起带 `version` 字段，结构变化时写迁移，不让旧文件直接报错。
- 每个安装实例生成唯一 ID 并持久保存。

## 测试

- 配置合并：golden file 测试（输入订阅 + 模板 + mixin，与期望输出逐字比对）。这是最该投入测试的地方。
- CLI 行为：`rogpeppe/go-internal` 的 testscript。
- nftables：在 network namespace 中做集成测试，不污染宿主机。
