// Package paths 定义 tunlr 在磁盘上的布局。所有路径都通过 Layout 获取，
// 测试时设置 Root，把整套布局放进临时目录。
package paths

import "path/filepath"

// Layout 是 tunlr 的磁盘布局。
//
//	/etc/tunlr/              用户可编辑：mixin、工具设置
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

// MixinFile 是用户的 mixin，即配置合成的第三层。
func (l Layout) MixinFile() string { return l.join("etc", "tunlr", "mixin.yaml") }

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

// KernelsDir 存放已知可用的内核版本，每个版本一个子目录。
func (l Layout) KernelsDir() string { return l.join("var", "lib", "tunlr", "kernels") }

// CoreDir 是 mihomo 的工作目录（-d），存放生效的配置、geo 数据、面板和缓存。
func (l Layout) CoreDir() string { return l.join("var", "lib", "tunlr", "core") }

// CoreConfig 是生效的 mihomo 配置文件。
func (l Layout) CoreConfig() string { return l.join("var", "lib", "tunlr", "core", "config.yaml") }

// LockFile 是所有会改动系统的操作共用的文件锁。放在 /run 下，未安装时也可用。
func (l Layout) LockFile() string { return l.join("run", "lock", "tunlr.lock") }
