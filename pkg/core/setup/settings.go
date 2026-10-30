package setup

import (
	"context"
	"errors"

	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// SettingsEntries 列出全部工具设置（tunlr settings）。
func (i *Installer) SettingsEntries() ([]settings.Entry, error) {
	set, err := settings.Load(i.Layout.SettingsFile())
	if err != nil {
		return nil, err
	}
	return set.Entries(subscription.DefaultUserAgent(i.Version)), nil
}

// SaveSettings 把编辑后的设置文件内容写入（见 settings.Write），随即按新的设置启用或停用 timer。
// 需要发布的改动由调用方随后发布。
func (i *Installer) SaveSettings(ctx context.Context, data, previous []byte, existed bool) error {
	l, err := i.lock(ctx)
	if err != nil {
		return err
	}
	err = settings.Write(i.Layout.SettingsFile(), data, previous, existed)
	// 释放锁之后再同步 unit：启用 tunlr-firewall.service 会执行 _firewall，它要取锁。
	l.Release()
	if err != nil {
		return err
	}
	if err := i.SyncTimers(ctx); err != nil {
		return errors.Join(err, settings.Restore(i.Layout.SettingsFile(), previous, existed))
	}
	return nil
}
