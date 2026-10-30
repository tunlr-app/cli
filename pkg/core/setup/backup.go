package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/backup"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/pipeline"
	"github.com/tunlr-app/cli/pkg/core/settings"
	"github.com/tunlr-app/cli/pkg/core/state"
	"github.com/tunlr-app/cli/pkg/core/subscription"
)

// keepBackups 是还原前自动备份保留的份数。
const keepBackups = 3

// ErrNotInitialized 表示这台机器还没有 tunlr 的状态，满足 errors.Is(err, core.ErrCheckFailed)。
var ErrNotInitialized = fmt.Errorf("%w: tunlr has not been set up here yet; run tunlr init first", core.ErrCheckFailed)

// Backup 收集当前的设置、订阅列表、mixin、订阅内容与状态中可以迁移的部分（见 backup.Backup）。
func (i *Installer) Backup(ctx context.Context) (*backup.Backup, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	return i.collect()
}

// collect 同 Backup，调用方须持有文件锁。
func (i *Installer) collect() (*backup.Backup, error) {
	st, err := state.Load(i.Layout.StateFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	b := &backup.Backup{
		Version: backup.Version, Tunlr: i.Version, Host: host, CreatedAt: i.now().UTC(),
		APISecret: st.APISecret, Devices: st.Devices, Filter: st.Filter,
		Switches: backup.Switches{Tun: st.TunEnabled(), ProxyLAN: st.ProxyLAN != nil, PanelLAN: st.PanelLAN != nil},
		Contents: map[string][]byte{},
	}
	if st.Gateway != nil {
		b.Switches.Gateway = st.Gateway.Interface
	}
	if st.LANAccess != nil {
		b.Switches.LANInterfaces, b.Switches.LANContainers = st.LANAccess.Interfaces, st.LANAccess.Containers
	}
	b.Settings, err = os.ReadFile(i.Layout.SettingsFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if b.Subscriptions, err = subscription.LoadList(i.Layout.SubscriptionList()); err != nil {
		return nil, err
	}
	b.List, err = os.ReadFile(i.Layout.SubscriptionList())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if b.Mixins, err = config.LoadAllMixins(i.Layout.MixinDir()); err != nil {
		return nil, err
	}
	for _, s := range b.Subscriptions.Subscriptions {
		data, err := os.ReadFile(i.Layout.SubscriptionFile(s.Name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		b.Contents[s.Name] = data
	}
	return b, nil
}

// PlanRestore 按还原 b 之后的样子合成配置并校验，什么都不写入。
func (i *Installer) PlanRestore(ctx context.Context, b *backup.Backup) (*pipeline.Plan, error) {
	if _, err := os.Stat(i.Layout.StateFile()); errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	set := &settings.Settings{Version: settings.Version}
	if b.Settings != nil {
		var err error
		if set, err = settings.Parse(b.Settings); err != nil {
			return nil, err
		}
	}
	return i.pipeline(nil).PlanRestore(ctx, pipeline.Restored{
		Settings: set, Mixins: b.Mixins, Active: b.Subscriptions.Active, Content: b.Contents[b.Subscriptions.Active],
		APISecret: b.APISecret, Devices: b.Devices, Filter: b.Filter,
	})
}

// RestoreResult 是还原的结果。
type RestoreResult struct {
	// Saved 是还原前自动备份的当前内容。
	Saved string `json:"saved"`
	// Revision 是发布的配置修订，配置没有变化时为 nil。
	Revision *config.Revision `json:"revision,omitempty"`
}

// Restore 还原备份 b，plan 是 PlanRestore 的结果（已经用户确认）：先把当前内容自动备份到 BackupsDir，
// 再写入设置、订阅列表、mixin、订阅内容和状态，然后发布。发布失败时写回自动备份的内容，配置由流水线回滚。
// 实例 ID、内核、开关和定时任务的记录保持本机的；订阅的抓取记录清空，各订阅在下次定时更新时重新抓取。
func (i *Installer) Restore(ctx context.Context, b *backup.Backup, plan *pipeline.Plan) (*RestoreResult, error) {
	l, err := i.lock(ctx)
	if err != nil {
		return nil, err
	}
	res, err := i.restoreLocked(ctx, b, plan)
	l.Release()
	if err != nil {
		return res, err
	}
	// 订阅的周期和配置中 geo 数据的自动更新可能变了。
	if err := i.SyncTimers(ctx); err != nil {
		return res, fmt.Errorf("the backup is restored, but updating the timers failed: %w", err)
	}
	return res, nil
}

func (i *Installer) restoreLocked(ctx context.Context, b *backup.Backup, plan *pipeline.Plan) (*RestoreResult, error) {
	cur, err := i.collect()
	if err != nil {
		return nil, err
	}
	saved, err := i.saveBackup(cur)
	if err != nil {
		return nil, fmt.Errorf("back up the current contents: %w", err)
	}
	res := &RestoreResult{Saved: saved}
	if err := i.write(b); err != nil {
		return nil, errors.Join(err, i.write(cur))
	}
	if !plan.Changed() {
		return res, nil
	}
	applied, err := i.pipeline(nil).ApplyLocked(ctx, plan)
	if err != nil {
		return nil, errors.Join(err, i.write(cur))
	}
	if applied != nil {
		res.Revision = applied.Revision
	}
	return res, nil
}

// saveBackup 把 b 写入 BackupsDir，只保留最近 keepBackups 份。
func (i *Installer) saveBackup(b *backup.Backup) (string, error) {
	dir := i.Layout.BackupsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "before-restore-"+b.CreatedAt.Format("20060102-150405.000000")+".tar.gz")
	if err := backup.WriteFile(path, b); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return path, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "before-restore-") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names) // 文件名中的时间可以按字符排序
	for len(names) > keepBackups {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return path, err
		}
		names = names[1:]
	}
	return path, nil
}

// write 把 b 写到磁盘：设置文件、订阅列表、mixin.d、订阅内容整体替换，状态文件只替换 b 中的部分。
func (i *Installer) write(b *backup.Backup) error {
	if err := replaceFile(i.Layout.SettingsFile(), b.Settings, 0o644); err != nil {
		return err
	}
	if err := replaceFile(i.Layout.SubscriptionList(), b.List, 0o600); err != nil {
		return err
	}

	if err := i.writeMixins(b.Mixins); err != nil {
		return err
	}
	files := map[string][]byte{}
	for name, data := range b.Contents {
		files[name+".yaml"] = data
	}
	if err := replaceYAML(i.Layout.SubscriptionsDir(), files); err != nil {
		return err
	}

	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		return err
	}
	st.Fetches, st.APISecret, st.Devices, st.Filter = nil, b.APISecret, b.Devices, b.Filter
	return state.Save(i.Layout.StateFile(), st)
}

// replaceFile 把 /etc/tunlr 中的文件 path 换成 data（权限 perm），data 为 nil 时删除它。
func replaceFile(path string, data []byte, perm os.FileMode) error {
	if data == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFile(path, data, perm)
}

// writeMixins 让 mixin.d 和其中订阅 mixin 的目录恰好是 mixins：备份中没有的订阅目录清空 .yaml 后删除
// （目录中还有别的文件时保留目录）。
func (i *Installer) writeMixins(mixins []config.Mixin) error {
	dir := i.Layout.MixinDir()
	byDir := map[string]map[string][]byte{"": {}}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && config.ValidMixinDir(e.Name()) {
				byDir[e.Name()] = map[string][]byte{}
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, m := range mixins {
		sub, file, ok := strings.Cut(m.Name, "/")
		if !ok {
			sub, file = "", m.Name
		}
		if byDir[sub] == nil {
			byDir[sub] = map[string][]byte{}
		}
		byDir[sub][file] = m.Data
	}
	if err := replaceYAML(dir, byDir[""]); err != nil {
		return err
	}
	for sub, files := range byDir {
		if sub == "" {
			continue
		}
		if err := replaceYAML(filepath.Join(dir, sub), files); err != nil {
			return err
		}
		if len(files) == 0 {
			os.Remove(filepath.Join(dir, sub)) // 不为空（有别的文件）时失败，保留
		}
	}
	return nil
}

// replaceYAML 让目录 dir 中的 .yaml 文件恰好是 files（0600，目录 0700）。以点开头的文件和子目录不动，
// 与 config.LoadMixins 读取的范围一致。
func replaceYAML(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		if _, keep := files[name]; !keep {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	for name, data := range files {
		if err := fsutil.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// SwitchSuggestion 是备份时开着、这台机器上关着的开关，还原后建议打开。
type SwitchSuggestion struct {
	Tun      bool `json:"tun,omitempty"`
	Gateway  bool `json:"gateway,omitempty"`
	ProxyLAN bool `json:"proxy_lan,omitempty"`
	PanelLAN bool `json:"panel_lan,omitempty"`
	// Interface 是网关要用的网卡：备份中的网卡在这台机器上也是局域网网卡的候选时用它，否则为空（默认路由
	// 所在的网卡）。Was 是备份中的网卡。
	Interface string `json:"interface,omitempty"`
	Was       string `json:"was,omitempty"`
	// LANInterfaces 是打开代理端口或面板的局域网访问时放行的网卡：备份中选过的、这台机器上也有的；都没有时
	// 为空（取默认值）。LANWas 是备份中的选择，LANContainers 是备份中是否放行容器。
	LANInterfaces []string `json:"lan_interfaces,omitempty"`
	LANWas        []string `json:"lan_was,omitempty"`
	LANContainers bool     `json:"lan_containers,omitempty"`
}

// Any 报告是否有要打开的开关。
func (s SwitchSuggestion) Any() bool { return s.Tun || s.Gateway || s.ProxyLAN || s.PanelLAN }

// SuggestSwitches 比较备份 b 与这台机器的开关，返回备份时开着、这里关着的。网关开着时 TUN 也开着，
// 打开网关会先打开 TUN，所以要打开网关时 Tun 为 false。
func (i *Installer) SuggestSwitches(ctx context.Context, b *backup.Backup) (SwitchSuggestion, error) {
	st, err := state.Load(i.Layout.StateFile())
	if err != nil {
		return SwitchSuggestion{}, err
	}
	sw := b.Switches
	s := SwitchSuggestion{
		Gateway:  sw.Gateway != "" && st.Gateway == nil,
		ProxyLAN: sw.ProxyLAN && st.ProxyLAN == nil,
		PanelLAN: sw.PanelLAN && st.PanelLAN == nil,
	}
	s.Tun = sw.Tun && !st.TunEnabled() && !s.Gateway
	if s.Gateway {
		s.Was = sw.Gateway
		cands, err := i.LANInterfaces(ctx)
		if err != nil {
			return s, err
		}
		if slices.ContainsFunc(cands, func(c LANInterface) bool { return c.Name == sw.Gateway }) {
			s.Interface = sw.Gateway
		}
	}
	if s.ProxyLAN || s.PanelLAN {
		s.LANWas, s.LANContainers = sw.LANInterfaces, sw.LANContainers
		for _, name := range sw.LANInterfaces {
			if _, err := i.lookupInterface(name); err == nil {
				s.LANInterfaces = append(s.LANInterfaces, name)
			}
		}
	}
	return s, nil
}

// SavedBackup 是一份自动备份。
type SavedBackup struct {
	Path   string         `json:"path"`
	Backup *backup.Backup `json:"-"`
	// Err 是读取失败的原因（文件损坏等），此时 Backup 为 nil。
	Err error `json:"-"`
}

// SavedBackups 列出 BackupsDir 中还原前自动备份的内容，最新的在前。
func (i *Installer) SavedBackups() ([]SavedBackup, error) {
	dir := i.Layout.BackupsDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved []SavedBackup
	for _, e := range slices.Backward(entries) {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "before-restore-") {
			continue
		}
		s := SavedBackup{Path: filepath.Join(dir, e.Name())}
		s.Backup, s.Err = backup.ReadFile(s.Path, nil)
		saved = append(saved, s)
	}
	return saved, nil
}
