// Package paths 定义 tunlr 在磁盘上的布局。所有路径都通过 Layout 获取，
// 测试时设置 Root，把整套布局放进临时目录。
package paths

import "path/filepath"

// Layout 是 tunlr 的磁盘布局。
//
//	/etc/tunlr/              用户可编辑：mixin（mixin.d/）、工具设置
//	/var/lib/tunlr/          工具状态：状态文件、订阅缓存、配置历史、已知可用内核
//	/var/lib/tunlr/core/     mihomo 工作目录（-d），归运行内核的专用用户所有
//	/run/lock/tunlr.lock     文件锁
type Layout struct {
	// Root 是所有路径的前缀，正常运行时为空，即从 / 开始。
	Root string
}

func (l Layout) join(elem ...string) string {
	return filepath.Join(append([]string{l.Root, "/"}, elem...)...)
}

// ConfigDir 存放用户可编辑的文件。
func (l Layout) ConfigDir() string { return l.join("etc", "tunlr") }

// SettingsFile 是工具设置，例如 GitHub 下载镜像。
func (l Layout) SettingsFile() string { return l.join("etc", "tunlr", "settings.yaml") }

// MixinDir 存放用户的 mixin 文件（*.yaml），按文件名顺序叠加在订阅之上。
func (l Layout) MixinDir() string { return l.join("etc", "tunlr", "mixin.d") }

// SubscriptionMixinDir 存放订阅 name 自己的 mixin，只在它启用时应用，排在共用的之后。
// name 须先经过校验，不能包含路径分隔符。
func (l Layout) SubscriptionMixinDir(name string) string {
	return l.join("etc", "tunlr", "mixin.d", name)
}

// DataDir 存放工具自己维护的状态，用户不需要编辑。
func (l Layout) DataDir() string { return l.join("var", "lib", "tunlr") }

// StateFile 是工具的状态文件，带结构版本号和安装实例 ID。
func (l Layout) StateFile() string { return l.join("var", "lib", "tunlr", "state.json") }

// SubscriptionsDir 存放每个订阅最近一次成功抓取的内容。
func (l Layout) SubscriptionsDir() string { return l.join("var", "lib", "tunlr", "subscriptions") }

// SubscriptionFile 是名为 name 的订阅最近一次成功抓取的内容。name 须先经过校验，
// 不能包含路径分隔符。
func (l Layout) SubscriptionFile(name string) string {
	return l.join("var", "lib", "tunlr", "subscriptions", name+".yaml")
}

// HistoryDir 存放已发布配置的历史，用于回滚。
func (l Layout) HistoryDir() string { return l.join("var", "lib", "tunlr", "history") }

// BackupsDir 存放还原备份前自动备份的当前内容。
func (l Layout) BackupsDir() string { return l.join("var", "lib", "tunlr", "backups") }

// KernelsDir 存放已知可用的内核版本，每个版本一个子目录。
func (l Layout) KernelsDir() string { return l.join("var", "lib", "tunlr", "kernels") }

// KernelBinary 是 version 版本内核的存放位置。version 须先经过校验，不能包含路径分隔符。
func (l Layout) KernelBinary(version string) string {
	return l.join("var", "lib", "tunlr", "kernels", version, "mihomo")
}

// BinDir 存放运行中的内核二进制。归运行内核的专用用户所有，面板触发的内核升级需要原地替换它。
func (l Layout) BinDir() string { return l.join("var", "lib", "tunlr", "bin") }

// CoreBinary 是运行中的内核，也是校验配置时使用的内核。
func (l Layout) CoreBinary() string { return l.join("var", "lib", "tunlr", "bin", "mihomo") }

// CoreDir 是 mihomo 的工作目录（-d），存放生效的配置、geo 数据、面板和缓存。
func (l Layout) CoreDir() string { return l.join("var", "lib", "tunlr", "core") }

// GeoStageDir 暂存来源变化、待发布时换进 CoreDir 的 geo 文件。与 CoreDir 在同一个文件系统上，替换用 rename。
func (l Layout) GeoStageDir() string { return l.join("var", "lib", "tunlr", "geo-staged") }

// CoreConfig 是生效的 mihomo 配置文件。
func (l Layout) CoreConfig() string { return l.join("var", "lib", "tunlr", "core", "config.yaml") }

// CoreSocketName 是内核 API 的 Unix socket（external-controller-unix），相对于工作目录。
// 内核以 0666 创建它且访问不需要 secret，访问控制依赖工作目录的权限。
const CoreSocketName = "api.sock"

// CoreSocket 是内核 API 的 Unix socket 的完整路径。
func (l Layout) CoreSocket() string { return l.join("var", "lib", "tunlr", "core", CoreSocketName) }

// LockFile 是所有会改动系统的操作共用的文件锁。放在 /run 下，未安装时也可用。
func (l Layout) LockFile() string { return l.join("run", "lock", "tunlr.lock") }

// UnitDir 存放工具生成的 systemd unit。
func (l Layout) UnitDir() string { return l.join("etc", "systemd", "system") }

// SysctlFile 是网关模式开启 IPv4 转发的 sysctl 配置，重启后仍生效。
func (l Layout) SysctlFile() string { return l.join("etc", "sysctl.d", "90-tunlr-gateway.conf") }

// IPForward 是 IPv4 转发的内核开关（procfs），测试时也放在 Root 下。
func (l Layout) IPForward() string { return l.join("proc", "sys", "net", "ipv4", "ip_forward") }

// RPFilter 是网卡 iface 的反向路径过滤设置（procfs）。iface 须先经过校验。
func (l Layout) RPFilter(iface string) string {
	return l.join("proc", "sys", "net", "ipv4", "conf", iface, "rp_filter")
}

// NetDevices 是网卡的 sysfs 目录（/sys/class/net），每块网卡一个子目录，测试时也放在 Root 下。
func (l Layout) NetDevices() string { return l.join("sys", "class", "net") }

// ToolBinary 是安装后的 tunlr 自身，unit 中引用这个路径。
func (l Layout) ToolBinary() string { return l.join("usr", "local", "bin", "tunlr") }
