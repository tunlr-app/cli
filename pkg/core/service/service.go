// Package service 负责 systemd unit 与 timer 的生成和控制，以及运行内核的系统用户。
// 工具生成的 unit 统一使用 tunlr- 前缀，控制操作直接执行 systemctl。
package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/tunlr-app/cli/pkg/core/fsutil"
	"github.com/tunlr-app/cli/pkg/core/paths"
	"github.com/tunlr-app/cli/templates"
)

// 工具生成的 unit。
const (
	CoreUnit    = "tunlr-core.service"
	RescueUnit  = "tunlr-rescue.service"
	UpdateUnit  = "tunlr-update.service"
	UpdateTimer = "tunlr-update.timer"
	CheckUnit   = "tunlr-check.service"
	CheckTimer  = "tunlr-check.timer"
	// AftercareUnit 由 AftercarePath 在 bin/mihomo 变化时触发，核对换过的内核（面板升级的善后）。
	AftercareUnit = "tunlr-aftercare.service"
	AftercarePath = "tunlr-aftercare.path"
	// FirewallUnit 随 nftables.service 启动、重启和重新加载，重新加载工具的表（它会清空所有表）。
	FirewallUnit = "tunlr-firewall.service"
)

// Units 是工具生成的全部 unit。
var Units = []string{CoreUnit, RescueUnit, UpdateUnit, UpdateTimer, CheckUnit, CheckTimer, AftercareUnit, AftercarePath, FirewallUnit}

// GuardUnit 是开启透明代理时布置的回滚定时器：systemd-run 建立的临时 unit（.timer 与 .service），
// 不写入磁盘，重启后消失（届时由 _prestart 处理过期的开关）。
const GuardUnit = "tunlr-tun-guard"

// CoreUser 是运行内核的系统用户（同名用户组）。
const CoreUser = "tunlr"

// Runner 执行外部命令，返回标准输出；失败时的错误包含标准错误的内容。
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec 是默认的 Runner。
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return out, nil
}

// Systemd 生成和控制工具的 systemd unit。
type Systemd struct {
	Layout paths.Layout
	// Run 为空时使用 Exec。
	Run Runner
}

// Status 是一个 unit 的运行状态（systemctl show 的部分属性）。
type Status struct {
	// LoadState 为 not-found 表示 unit 不存在。
	LoadState string `json:"load_state"`
	// ActiveState 如 active、inactive、failed、activating。
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
	MainPID     int    `json:"main_pid"`
	// NRestarts 是上次手动启动以来 systemd 自动重启的次数。
	NRestarts int `json:"n_restarts"`
}

// Active 报告 unit 是否在运行。
func (s Status) Active() bool { return s.ActiveState == "active" }

// Installed 报告内核的 unit 是否已安装。
func (s *Systemd) Installed() bool {
	_, err := os.Stat(filepath.Join(s.Layout.UnitDir(), CoreUnit))
	return err == nil
}

// Status 返回 unit 的运行状态。
func (s *Systemd) Status(ctx context.Context, unit string) (Status, error) {
	out, err := s.systemctl(ctx, "show", "--property=LoadState,ActiveState,SubState,MainPID,NRestarts", unit)
	if err != nil {
		return Status{}, err
	}
	var st Status
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "LoadState":
			st.LoadState = v
		case "ActiveState":
			st.ActiveState = v
		case "SubState":
			st.SubState = v
		case "MainPID":
			st.MainPID, _ = strconv.Atoi(v)
		case "NRestarts":
			st.NRestarts, _ = strconv.Atoi(v)
		}
	}
	return st, nil
}

// Property 返回 unit 的一个属性值，例如 timer 的 NextElapseUSecRealtime。
func (s *Systemd) Property(ctx context.Context, unit, name string) (string, error) {
	out, err := s.systemctl(ctx, "show", "--property="+name, "--value", unit)
	return strings.TrimSpace(string(out)), err
}

// Start 启动 unit。
func (s *Systemd) Start(ctx context.Context, unit string) error {
	_, err := s.systemctl(ctx, "start", unit)
	return err
}

// Stop 停止 unit。
func (s *Systemd) Stop(ctx context.Context, unit string) error {
	_, err := s.systemctl(ctx, "stop", unit)
	return err
}

// Restart 重启 unit（没有运行时启动它）。
func (s *Systemd) Restart(ctx context.Context, unit string) error {
	_, err := s.systemctl(ctx, "restart", unit)
	return err
}

// ResetFailed 清除 unit 的 failed 状态和重启计数，之后才能再次启动达到重启上限的服务。
func (s *Systemd) ResetFailed(ctx context.Context, unit string) error {
	_, err := s.systemctl(ctx, "reset-failed", unit)
	return err
}

// Outdated 返回磁盘上内容与现在的模板不同（或还不存在）的 unit，只读取。
func (s *Systemd) Outdated() ([]string, error) {
	units, err := RenderUnits(s.Layout)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range Units {
		if old, err := os.ReadFile(filepath.Join(s.Layout.UnitDir(), name)); err != nil || !bytes.Equal(old, units[name]) {
			out = append(out, name)
		}
	}
	return out, nil
}

// Install 写入全部 unit，重新加载 systemd，启用内核服务（不启动）。timer 是否启用由调用方按设置和
// 状态用 SetTimer 决定。返回内容有变化（含新建）的 unit：内核服务的 unit 变了，要重启内核才生效。
func (s *Systemd) Install(ctx context.Context) (changed []string, err error) {
	units, err := RenderUnits(s.Layout)
	if err != nil {
		return nil, err
	}
	if changed, err = s.Outdated(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Layout.UnitDir(), 0o755); err != nil {
		return nil, err
	}
	for _, name := range changed {
		if err := fsutil.WriteFile(filepath.Join(s.Layout.UnitDir(), name), units[name], 0o644); err != nil {
			return changed, err
		}
	}
	if _, err := s.systemctl(ctx, "daemon-reload"); err != nil {
		return changed, err
	}
	_, err = s.systemctl(ctx, "enable", CoreUnit)
	return changed, err
}

// SetTimer 启用并启动（on）或停止并禁用 timer（或 path unit）。只启用不启动的话，要到下次开机才开始计时。
// 已经是目标状态时 systemctl 什么都不做。
func (s *Systemd) SetTimer(ctx context.Context, timer string, on bool) error {
	action := "disable"
	if on {
		action = "enable"
	}
	_, err := s.systemctl(ctx, action, "--now", timer)
	return err
}

// TimerEnabled 报告 timer 是否启用。
func (s *Systemd) TimerEnabled(ctx context.Context, timer string) bool {
	v, err := s.Property(ctx, timer, "UnitFileState")
	return err == nil && v == "enabled"
}

// Uninstall 停止并禁用全部 unit，删除 unit 文件，重新加载 systemd。unit 不存在时不报错。
func (s *Systemd) Uninstall(ctx context.Context) error {
	var errs []error
	s.DisarmGuard(ctx)
	if _, err := s.systemctl(ctx, "disable", "--now", UpdateTimer, CheckTimer, AftercarePath, FirewallUnit, CoreUnit); err != nil && s.Installed() {
		errs = append(errs, err)
	}
	for _, name := range Units {
		if err := os.Remove(filepath.Join(s.Layout.UnitDir(), name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if _, err := s.systemctl(ctx, "daemon-reload"); err != nil {
		errs = append(errs, err)
	}
	if _, err := s.systemctl(ctx, "reset-failed", CoreUnit, RescueUnit, UpdateUnit, CheckUnit, AftercareUnit, FirewallUnit); err != nil {
		// unit 已经删除且没有 failed 状态时 systemctl 报告找不到，不算失败。
		_ = err
	}
	return errors.Join(errs...)
}

// ArmGuard 布置回滚定时器：after 之后执行 tunlr _tun-guard。已有的定时器先撤掉。
func (s *Systemd) ArmGuard(ctx context.Context, after time.Duration) error {
	s.DisarmGuard(ctx)
	_, err := s.run(ctx, "systemd-run", "--quiet", "--unit="+GuardUnit,
		"--on-active="+strconv.Itoa(int(after.Seconds())), "--timer-property=AccuracySec=1s",
		"--description=Turn the transparent proxy off unless confirmed (tunlr)",
		s.Layout.ToolBinary(), "_tun-guard")
	return err
}

// DisarmGuard 撤掉回滚定时器，不存在时什么都不做。只停止 .timer：回滚本身（.service）可能正是调用者。
func (s *Systemd) DisarmGuard(ctx context.Context) {
	// 定时器不存在时 systemctl 报告找不到，不算失败。
	_, _ = s.systemctl(ctx, "stop", GuardUnit+".timer")
	_, _ = s.systemctl(ctx, "reset-failed", GuardUnit+".timer", GuardUnit+".service")
}

// GuardArmed 报告回滚定时器是否在计时。
func (s *Systemd) GuardArmed(ctx context.Context) bool {
	st, err := s.Status(ctx, GuardUnit+".timer")
	return err == nil && st.Active()
}

// Logs 把内核服务的日志（journalctl）写到 stdout。follow 时持续输出，直到 ctx 结束。
func (s *Systemd) Logs(ctx context.Context, lines int, follow bool, stdout, stderr io.Writer) error {
	args := []string{"--unit", CoreUnit, "--no-pager", "--lines", strconv.Itoa(lines)}
	if follow {
		args = append(args, "--follow")
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if follow && ctx.Err() != nil {
		return nil
	}
	return err
}

// RecentLog 返回内核服务最近 n 行与连接无关的日志（去掉 [TCP]、[UDP] 的连接记录，它们几分钟就能
// 把其他内容冲掉），每行缩进两格，用于解释加载配置卡住之类的故障。读不到时返回空字符串。
func RecentLog(ctx context.Context, n int) string {
	out, err := exec.CommandContext(ctx, "journalctl", "--unit", CoreUnit, "--no-pager", "--output", "cat", "--lines", "2000").Output()
	if err != nil {
		return ""
	}
	return tailWithoutConnections(string(out), n)
}

// tailWithoutConnections 取 log 中最后 n 行不是连接记录的行，每行缩进两格。
func tailWithoutConnections(log string, n int) string {
	var kept []string
	for line := range strings.SplitSeq(strings.TrimRight(log, "\n"), "\n") {
		if line == "" || strings.Contains(line, "[TCP]") || strings.Contains(line, "[UDP]") {
			continue
		}
		kept = append(kept, "  "+line)
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}

// RenderUnits 按布局生成全部 unit 的内容。
func RenderUnits(l paths.Layout) (map[string][]byte, error) {
	data := struct {
		Tool, Binary, BinDir, CoreDir, User string
	}{l.ToolBinary(), l.CoreBinary(), l.BinDir(), l.CoreDir(), CoreUser}
	units := map[string][]byte{}
	for _, name := range Units {
		src, err := templates.Systemd.ReadFile("systemd/" + name)
		if err != nil {
			return nil, err
		}
		t, err := template.New(name).Option("missingkey=error").Parse(string(src))
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err := t.Execute(&b, data); err != nil {
			return nil, err
		}
		units[name] = b.Bytes()
	}
	return units, nil
}

// KernelUser 返回布局 l 中内核文件的属主，也是 mihomo -t 的运行身份：内核用户，尚未创建时为 nil。
// 测试用的布局（Root 不为空）不涉及系统用户，也返回 nil。
func KernelUser(l paths.Layout) (*fsutil.Owner, error) {
	if l.Root != "" {
		return nil, nil
	}
	return LookupUser()
}

// LookupUser 返回运行内核的用户，尚未创建时返回 nil。
func LookupUser() (*fsutil.Owner, error) {
	if runtime.GOOS == "windows" {
		// 只在开发机上运行测试时出现；Windows 的查找失败不是 UnknownUserError。
		return nil, nil
	}
	u, err := user.Lookup(CoreUser)
	if errors.As(err, new(user.UnknownUserError)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, err
	}
	return &fsutil.Owner{UID: uid, GID: gid}, nil
}

// EnsureUser 创建运行内核的系统用户（已存在时直接返回），不能登录，没有主目录。
func (s *Systemd) EnsureUser(ctx context.Context) (*fsutil.Owner, error) {
	if o, err := LookupUser(); err != nil || o != nil {
		return o, err
	}
	if _, err := s.run(ctx, "useradd", "--system", "--user-group", "--no-create-home",
		"--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", "--comment", "tunlr mihomo kernel", CoreUser); err != nil {
		return nil, err
	}
	o, err := LookupUser()
	if err == nil && o == nil {
		err = fmt.Errorf("user %s not found after creating it", CoreUser)
	}
	return o, err
}

// RemoveUser 删除运行内核的用户，不存在时什么都不做。
func (s *Systemd) RemoveUser(ctx context.Context) error {
	if o, err := LookupUser(); err != nil || o == nil {
		return err
	}
	_, err := s.run(ctx, "userdel", CoreUser)
	return err
}

func (s *Systemd) systemctl(ctx context.Context, args ...string) ([]byte, error) {
	return s.run(ctx, "systemctl", args...)
}

func (s *Systemd) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if s.Run != nil {
		return s.Run(ctx, name, args...)
	}
	return Exec(ctx, name, args...)
}
