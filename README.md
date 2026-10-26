# tunlr

tunlr（读作 “TUN-ler”，tun + -lr）是在 Linux 服务器上运行 [mihomo](https://github.com/MetaCubeX/mihomo) 的运维工具：负责内核安装与升级、systemd 托管、配置编排与回滚、透明代理开关和环境诊断。切节点、测速、连接与日志等运行时交互交给 [zashboard](https://github.com/Zephyruso/zashboard)。

> 本项目与 MetaCubeX 无关，仅在描述用途时提及 mihomo。

> 项目处于早期开发阶段，尚不可用。

## 支持范围

- 使用 systemd 的 Linux（Debian 12+、Ubuntu 22.04+ 等）
- amd64、arm64
- nftables
- 以 root 运行

## 开发

```bash
go test ./...
```

```bash
goreleaser build --snapshot --clean
```

设计文档见 [docs/DESIGN.md](docs/DESIGN.md)。
