package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/dlclark/regexp2"
	"go.yaml.in/yaml/v3"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// Mixin 是一份用户 mixin 文件。
//
// mixin 的顶层只有两个命名空间（DESIGN.md 第 6 节）：
//
//	mihomo:  mihomo 的原生配置，原样合并：映射逐层合并，标量和列表整体替换，值为 null 时删除该键
//	tunlr:   tunlr 的语法，不写入配置：节点筛选 proxy-filter，以及对订阅的 proxies、proxy-groups、
//	         rules、sub-rules.<名称> 的操作，各有四种：
//	           prepend / append  在列表前插入 / 后追加；规则追加在末尾的 MATCH 之前
//	           patch             按 name 找到列表中的项并修改它，项中可用 prepend-<字段> 等前缀操作其中的列表
//	           delete            按 name（规则按内容）删除；删除节点或策略组时，同时从所有策略组的 proxies 中移除
//
// name 写成 /正则/ 时匹配所有符合的项。一个文件中先合并 mihomo:，再按 删除 → 修改 → 插入 的顺序
// 执行 tunlr: 的操作，与书写顺序无关。顶层以 x- 开头的键不写入配置，用来放 YAML 锚点。
//
// Name 中带一级目录（如 home/10-filter.yaml）的是订阅 mixin，位于 mixin.d/<订阅名>/。
type Mixin struct {
	// Name 是文件名，如 20-home.yaml，用于提示和报错。
	Name string
	Data []byte
}

// Warning 是合成时发现、但不阻止发布的问题。
type Warning struct {
	// File 是相关的 mixin 文件，与具体文件无关时为空。
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

func (w Warning) String() string {
	switch {
	case w.File == "":
		return w.Message
	case w.Line == 0:
		return w.File + ": " + w.Message
	}
	return fmt.Sprintf("%s:%d: %s", w.File, w.Line, w.Message)
}

// Override 记录后面的 mixin 文件覆盖了前面文件设置的同一个键。
type Override struct {
	// Path 是键的路径，如 dns.nameserver、proxy-groups[节点选择].interval。
	Path     string `json:"path"`
	File     string `json:"file"`
	Previous string `json:"previous"`
}

// MixinError 是 mixin 文件中的错误。有错误的 mixin 不能发布，
// 满足 errors.Is(err, core.ErrCheckFailed)。
type MixinError struct {
	File string
	// Line 为零表示没有具体的行，或行号已包含在 Err 中（YAML 解析错误）。
	Line int
	Err  error
}

func (e *MixinError) Error() string {
	if e.Line == 0 {
		return fmt.Sprintf("mixin %s: %v", e.File, e.Err)
	}
	return fmt.Sprintf("mixin %s:%d: %v", e.File, e.Line, e.Err)
}

func (e *MixinError) Unwrap() error { return core.ErrCheckFailed }

// mixinExt 是 mixin 文件的扩展名。
const mixinExt = ".yaml"

var mixinName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// MixinFileName 把用户给出的名称（可带或不带 .yaml）转换为 mixin 文件名。名称写成 <订阅名>/<文件名>
// 时是订阅 mixin，结果也带着订阅名（如 home/10-filter.yaml）；订阅名的规则与订阅相同。
func MixinFileName(name string) (string, error) {
	sub, file := splitMixinName(name)
	stem := strings.TrimSuffix(file, mixinExt)
	if !mixinName.MatchString(stem) || (strings.Contains(name, "/") && !mixinName.MatchString(sub)) {
		return "", fmt.Errorf("%w: invalid mixin name %q: use letters, digits, '.', '_' and '-' (e.g. 20-rules, or home/10-filter for subscription home)",
			core.ErrInvalidArgument, name)
	}
	if sub != "" {
		return sub + "/" + stem + mixinExt, nil
	}
	return stem + mixinExt, nil
}

// ValidMixinDir 报告 name 能否作为订阅 mixin 的目录名（与订阅名的规则相同）。
func ValidMixinDir(name string) bool { return mixinName.MatchString(name) }

// MixinPath 返回名为 name（MixinFileName 的结果）的 mixin 在 dir（mixin.d）中的路径。
func MixinPath(dir, name string) string {
	return filepath.Join(dir, filepath.FromSlash(name))
}

// LoadMixinsFor 读取共用的 mixin 和订阅 sub 的 mixin（mixin.d/<sub>/，名称带上 "<sub>/"），
// 按应用的顺序：先是共用的，再是订阅的，各自按文件名排序。sub 为空时只读取共用的。
func LoadMixinsFor(dir, sub string) ([]Mixin, error) {
	mixins, err := LoadMixins(dir)
	if err != nil || sub == "" {
		return mixins, err
	}
	own, err := LoadMixins(filepath.Join(dir, sub))
	if err != nil {
		return nil, err
	}
	for _, m := range own {
		mixins = append(mixins, Mixin{Name: sub + "/" + m.Name, Data: m.Data})
	}
	return mixins, nil
}

// LoadAllMixins 读取共用的 mixin 和所有订阅的 mixin（不论订阅是否存在），共用的在前，订阅的按订阅名排序。
// 用于列出、备份。
func LoadAllMixins(dir string) ([]Mixin, error) {
	mixins, err := LoadMixins(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !ValidMixinDir(e.Name()) {
			continue
		}
		own, err := LoadMixins(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		for _, m := range own {
			mixins = append(mixins, Mixin{Name: e.Name() + "/" + m.Name, Data: m.Data})
		}
	}
	return mixins, nil
}

// LoadMixins 读取目录中的 mixin 文件，按文件名排序。只读取扩展名为 .yaml 的文件；
// 以点开头的文件（如编辑器的临时文件）和子目录被忽略（订阅 mixin 见 LoadMixinsFor）。
// 目录不存在时返回空。
func LoadMixins(dir string) ([]Mixin, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var mixins []Mixin
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, mixinExt) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		mixins = append(mixins, Mixin{Name: name, Data: data})
	}
	return mixins, nil
}

var numberPrefix = regexp.MustCompile(`^[0-9]+`)

// orderWarnings 提醒数字前缀位数不同导致的意外顺序：文件名逐字符排序，
// 9-a.yaml 排在 10-b.yaml 之后。mixins 在同一目录内已按文件名排序，只比较同一目录中的文件。
func orderWarnings(mixins []Mixin) []Warning {
	var ws []Warning
	for i, b := range mixins {
		dirB, fileB := splitMixinName(b.Name)
		pb := numberPrefix.FindString(fileB)
		if pb == "" {
			continue
		}
		for _, a := range mixins[:i] {
			dirA, fileA := splitMixinName(a.Name)
			pa := numberPrefix.FindString(fileA)
			if dirA != dirB || pa == "" || compareNumbers(pa, pb) <= 0 {
				continue
			}
			pad := strings.Repeat("0", max(len(pa)-len(pb), 0))
			ws = append(ws, Warning{File: b.Name, Message: fmt.Sprintf(
				"applied after %s because names sort character by character; to apply it before, rename it to %s",
				a.Name, pad+fileB)})
			break
		}
	}
	return ws
}

// splitMixinName 把 mixin 的名称拆成订阅名（共用的为空）和文件名。
func splitMixinName(name string) (sub, file string) {
	if sub, file, ok := strings.Cut(name, "/"); ok {
		return sub, file
	}
	return "", name
}

// compareNumbers 比较两个十进制数字串的数值，不受长度限制。
func compareNumbers(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}

// ErrMixinChanged 表示 mixin 文件在编辑期间被别人改动过。
var ErrMixinChanged = errors.New("the mixin file was changed by someone else while you were editing it")

// SaveMixin 原子写入 mixin 文件。previous 和 existed 是开始编辑时读到的内容和文件是否存在；
// 文件此后被改动过时返回 ErrMixinChanged，不覆盖别人的修改。
// mixin 可能含有节点密码，目录为 0700，文件为 0600。
func SaveMixin(dir string, m Mixin, previous []byte, existed bool) error {
	path := MixinPath(dir, m.Name)
	cur, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if existed {
			return ErrMixinChanged
		}
	case err != nil:
		return err
	case !existed || !bytes.Equal(cur, previous):
		return ErrMixinChanged
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFile(path, m.Data, 0o600)
}

// mihomoKey 是 mixin 顶层中 mihomo 原生配置的命名空间，原样合并进生成的配置。
const mihomoKey = "mihomo"

// 列表操作，按执行顺序排列。
type mixinOp int

const (
	opDelete mixinOp = iota
	opPatch
	opInsert // prepend 与 append
)

// listOp 是一种列表操作的写法。
type listOp struct {
	key     string
	op      mixinOp
	prepend bool
}

// listOps 是 tunlr: 下每个列表可用的操作，按执行顺序。
var listOps = []listOp{
	{"delete", opDelete, false},
	{"patch", opPatch, false},
	{"prepend", opInsert, true},
	{"append", opInsert, false},
}

// tunlrLists 是 tunlr: 下可以操作的订阅列表（另有 sub-rules.<名称>）。
var tunlrLists = []string{"proxies", "proxy-groups", "rules"}

// mixer 把 mixin 文件逐个应用到配置上，记录警告和文件之间的覆盖。
type mixer struct {
	root *yaml.Node
	// file 是正在应用的文件。
	file string
	// subKeys 是来自订阅的顶层键。
	subKeys map[string]bool
	// setBy 记录普通键的路径最近由哪个文件设置。
	setBy map[string]string
	// fallback 记录 setBy 中最近由 tunlr.defaults 设置的路径；defaults 为 true 时正在应用 tunlr.defaults。
	fallback map[string]bool
	defaults bool
	// filled 记录订阅的映射中缺少、由兜底值补上的路径（见 fillFiles）。
	filled    map[string]bool
	warnings  []Warning
	overrides []Override
}

func (m *mixer) warnf(line int, format string, args ...any) {
	m.warnings = append(m.warnings, Warning{File: m.file, Line: line, Message: fmt.Sprintf(format, args...)})
}

func (m *mixer) errorf(line int, format string, args ...any) error {
	return &MixinError{File: m.file, Line: line, Err: fmt.Errorf(format, args...)}
}

// record 记录 path 由当前文件设置；之前由另一个文件设置过时记为覆盖。
func (m *mixer) record(path string) {
	if prev, ok := m.setBy[path]; ok && prev != m.file {
		m.overrides = append(m.overrides, Override{Path: path, File: m.file, Previous: prev})
	}
	m.setBy[path] = m.file
	if m.fallback != nil {
		if m.defaults {
			m.fallback[path] = true
		} else {
			delete(m.fallback, path)
		}
	}
}

// applyDefaults 把 mixin src（已由 checkMixin 检查）的 tunlr.defaults 合并进 root，在订阅之前调用。
// 兜底值的优先级最低，写了受管 mixin 独占的键也不算冲突：独占的键最后写入，总是压过它（例如开启 TUN 时
// tunlr:tun 的 dns.enable）。
func (m *mixer) applyDefaults(root, src *yaml.Node) error {
	d := lookup(src, []string{namespace, "defaults"})
	if d == nil || d.Kind != yaml.MappingNode {
		return nil
	}
	m.defaults = true
	defer func() { m.defaults = false }()
	return m.merge(root, d, "")
}

// snapshot 返回 keys 中各键在 root 中当前的值（拷贝），不存在的为 nil。
func snapshot(root *yaml.Node, keys []owned) []*yaml.Node {
	before := make([]*yaml.Node, len(keys))
	for i, k := range keys {
		if n := lookup(root, k.path); n != nil {
			before[i] = clone(n)
		}
	}
	return before
}

// pending 是一个待执行的列表操作。
type pending struct {
	op      mixinOp
	prepend bool
	// label 是提示中操作的写法，如 tunlr.rules.prepend、prepend-proxies；line 是它所在的行。
	label string
	line  int
	// dst 是列表所在的映射，key 是列表的键，path 是 dst 在配置中的路径（顶层为空）。
	dst   *yaml.Node
	key   string
	path  string
	value *yaml.Node
}

// checkMixin 检查 mixin 文件的结构：顶层只有 mihomo、tunlr 和 x- 开头的键，mihomo 是映射，
// tunlr 中只有认识的键（见 checkNamespace）。
func (m *mixer) checkMixin(src *yaml.Node) error {
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		switch {
		case strings.HasPrefix(k.Value, "x-"):
		case k.Value == mihomoKey:
			if !isNull(v) && v.Kind != yaml.MappingNode {
				return m.errorf(v.Line, "%s must be a mapping", mihomoKey)
			}
		case k.Value == namespace:
			if err := m.checkNamespace(v); err != nil {
				return err
			}
		default:
			return m.errorf(k.Line, "%s: unknown top-level key; mihomo settings go under %s:, tunlr's operations under %s:",
				k.Value, mihomoKey, namespace)
		}
	}
	return nil
}

// applyFile 应用一个 mixin 文件（已由 checkMixin 检查）：先把 mihomo: 合并进配置，再执行 tunlr: 中的
// 列表操作（删除 → 修改 → 插入）。tunlr.proxy-filter 不在这里执行，见 filterProxies。
func (m *mixer) applyFile(root, src *yaml.Node) error {
	if v := lookup(src, []string{mihomoKey}); v != nil && v.Kind == yaml.MappingNode {
		if err := m.merge(root, v, ""); err != nil {
			return err
		}
	}
	t := lookup(src, []string{namespace})
	if t == nil || t.Kind != yaml.MappingNode {
		return nil
	}
	var ops []pending
	// sub-rules 不存在时先用一个独立的映射，插入了规则才放进配置，删除和修改不留下空的 sub-rules。
	var detached *yaml.Node
	for i := 0; i+1 < len(t.Content); i += 2 {
		k, v := t.Content[i], t.Content[i+1]
		switch {
		case slices.Contains(tunlrLists, k.Value):
			ops = append(ops, opsOf(root, k.Value, "", namespace+"."+k.Value, v)...)
		case k.Value == "sub-rules" && v.Kind == yaml.MappingNode:
			sr := lookup(root, []string{"sub-rules"})
			if sr == nil {
				detached = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				sr = detached
			}
			if sr.Kind != yaml.MappingNode {
				return m.errorf(k.Line, "%s.sub-rules: sub-rules is not a mapping", namespace)
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				name := v.Content[j].Value
				ops = append(ops, opsOf(sr, name, "sub-rules", namespace+".sub-rules."+name, v.Content[j+1])...)
			}
		}
	}
	if err := m.run(ops); err != nil {
		return err
	}
	if detached != nil && len(detached.Content) > 0 {
		setValue(root, "sub-rules", detached)
	}
	return nil
}

// opsOf 列出 tunlr: 下一个列表（如 tunlr.rules）中的操作。v 已由 checkOps 检查。
func opsOf(dst *yaml.Node, key, path, label string, v *yaml.Node) []pending {
	if v.Kind != yaml.MappingNode {
		return nil
	}
	var ops []pending
	for i := 0; i+1 < len(v.Content); i += 2 {
		k := v.Content[i]
		for _, op := range listOps {
			if k.Value == op.key {
				ops = append(ops, pending{op: op.op, prepend: op.prepend, label: label + "." + op.key, line: k.Line,
					dst: dst, key: key, path: path, value: v.Content[i+1]})
			}
		}
	}
	return ops
}

// merge 把 mihomo: 的映射 src 合并到配置映射 dst 上：null 删除，映射逐层合并，其他值整体替换，
// 不识别任何前缀操作。path 是 dst 在配置中的路径，顶层为空。
func (m *mixer) merge(dst, src *yaml.Node, path string) error {
	for i := 0; i+1 < len(src.Content); i += 2 {
		if err := m.set(dst, src.Content[i], src.Content[i+1], path, false); err != nil {
			return err
		}
	}
	return nil
}

// apply 把 patch 项的内容 src 应用到列表项 dst 上：普通键与 merge 相同，另外认识 prepend-<字段>、
// append-<字段>、patch-<字段>、delete-<字段> 前缀，作用于项中的列表字段（如策略组的 prepend-proxies），
// 同样按 删除 → 修改 → 插入 的顺序执行。
func (m *mixer) apply(dst, src *yaml.Node, path string) error {
	var ops []pending
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		op, prepend, key, ok := parseOp(k.Value)
		if !ok {
			if err := m.set(dst, k, v, path, true); err != nil {
				return err
			}
			continue
		}
		if key == "" {
			return m.errorf(k.Line, "%s: missing the key to operate on", k.Value)
		}
		ops = append(ops, pending{op: op, prepend: prepend, label: k.Value, line: k.Line, dst: dst, key: key, path: path, value: v})
	}
	return m.run(ops)
}

// run 按 删除 → 修改 → 插入 的顺序执行 ops，同类操作保持书写顺序。
func (m *mixer) run(ops []pending) error {
	slices.SortStableFunc(ops, func(a, b pending) int { return int(a.op) - int(b.op) })
	for _, o := range ops {
		if isNull(o.value) {
			continue
		}
		if o.value.Kind != yaml.SequenceNode {
			return m.errorf(o.line, "%s: value must be a list", o.label)
		}
		var err error
		switch o.op {
		case opDelete:
			err = m.delete(o)
		case opPatch:
			err = m.patch(o)
		case opInsert:
			err = m.insert(o)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// parseOp 识别 patch 项中的前缀操作，返回操作、是否为 prepend 和被操作的键。
func parseOp(k string) (op mixinOp, prepend bool, key string, ok bool) {
	for _, p := range listOps {
		if key, ok := strings.CutPrefix(k, p.key+"-"); ok {
			return p.op, p.prepend, key, true
		}
	}
	return 0, false, "", false
}

// set 处理普通键：null 删除，映射逐层合并，其他值整体替换。ops 为 true 时（patch 项中）
// 下一层映射同样认识前缀操作。
func (m *mixer) set(dst, k, v *yaml.Node, path string, ops bool) error {
	p := joinPath(path, k.Value)
	j := valueIndex(dst, k.Value)
	switch {
	case isNull(v):
		if j >= 0 {
			dst.Content = slices.Delete(dst.Content, j-1, j+1)
		}
		m.record(p)
		return nil
	case v.Kind == yaml.MappingNode:
		if j < 0 || dst.Content[j].Kind != yaml.MappingNode {
			if j >= 0 {
				m.record(p)
			}
			// 从空映射开始合并，使新映射中的 null（和 patch 项中的前缀操作）同样生效。
			setValue(dst, k.Value, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
			j = valueIndex(dst, k.Value)
		}
		if ops {
			return m.apply(dst.Content[j], v, p)
		}
		return m.merge(dst.Content[j], v, p)
	}
	if path == "" && j >= 0 && m.subKeys[k.Value] && v.Kind == yaml.SequenceNode {
		m.warnf(k.Line, "%s.%s replaces the subscription's %s entirely; use %s.%s (prepend, append, patch, delete) to keep them",
			mihomoKey, k.Value, k.Value, namespace, k.Value)
	}
	m.record(p)
	setValue(dst, k.Value, clone(v))
	return nil
}

// insert 处理 prepend 与 append，列表不存在时创建。
func (m *mixer) insert(o pending) error {
	j := valueIndex(o.dst, o.key)
	if j < 0 {
		setValue(o.dst, o.key, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
		j = valueIndex(o.dst, o.key)
	}
	target := o.dst.Content[j]
	if target.Kind != yaml.SequenceNode {
		return m.errorf(o.line, "%s: %s is not a list", o.label, joinPath(o.path, o.key))
	}
	add := make([]*yaml.Node, len(o.value.Content))
	for i, it := range o.value.Content {
		add[i] = clone(it)
	}
	at := 0
	if !o.prepend {
		at = len(target.Content)
		// 规则在 MATCH 之后永远不会命中，追加的规则放在末尾的 MATCH 之前。
		isRules := (o.path == "" && o.key == "rules") || o.path == "sub-rules"
		if isRules && at > 0 && isMatchRule(target.Content[at-1]) {
			at--
		}
	}
	target.Content = slices.Insert(target.Content, at, add...)
	return nil
}

// patch 处理 patch：每一项按 name 匹配列表中的项，把其余字段应用到匹配的每一项上（见 apply）。
func (m *mixer) patch(o pending) error {
	var target *yaml.Node
	if j := valueIndex(o.dst, o.key); j >= 0 {
		target = o.dst.Content[j]
		if target.Kind != yaml.SequenceNode {
			return m.errorf(o.line, "%s: %s is not a list", o.label, joinPath(o.path, o.key))
		}
	}
	for _, it := range o.value.Content {
		ni := -1
		if it.Kind == yaml.MappingNode {
			ni = valueIndex(it, "name")
		}
		if ni < 0 || it.Content[ni].Kind != yaml.ScalarNode {
			return m.errorf(it.Line, "%s: each item must be a mapping with a name", o.label)
		}
		match, err := m.matcher(o.label, it.Content[ni])
		if err != nil {
			return err
		}
		body := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map",
			Content: slices.Delete(slices.Clone(it.Content), ni-1, ni+1)}
		matched := false
		if target != nil {
			for _, t := range target.Content {
				if name, ok := itemName(t); ok && t.Kind == yaml.MappingNode && match.match(name) {
					matched = true
					if err := m.apply(t, body, fmt.Sprintf("%s[%s]", joinPath(o.path, o.key), name)); err != nil {
						return err
					}
				}
			}
		}
		if !matched {
			m.warnf(it.Line, "%s: no item named %s; nothing changed", o.label, match)
		}
	}
	return nil
}

// delete 处理 delete：列表按 name（字符串按值）删除，映射按键删除。
func (m *mixer) delete(o pending) error {
	items := o.value
	matchers := make([]matcher, len(items.Content))
	for i, it := range items.Content {
		var err error
		if matchers[i], err = m.matcher(o.label, it); err != nil {
			return err
		}
	}
	hit := make([]bool, len(matchers))
	matches := func(name string) bool {
		found := false
		for i, mt := range matchers {
			if mt.match(name) {
				hit[i], found = true, true
			}
		}
		return found
	}

	var removed []string
	if j := valueIndex(o.dst, o.key); j >= 0 {
		switch target := o.dst.Content[j]; target.Kind {
		case yaml.SequenceNode:
			target.Content = slices.DeleteFunc(target.Content, func(n *yaml.Node) bool {
				name, ok := itemName(n)
				if ok && matches(name) {
					removed = append(removed, name)
					return true
				}
				return false
			})
		case yaml.MappingNode:
			kept := target.Content[:0]
			for i := 0; i+1 < len(target.Content); i += 2 {
				if matches(target.Content[i].Value) {
					continue
				}
				kept = append(kept, target.Content[i], target.Content[i+1])
			}
			target.Content = kept
		default:
			return m.errorf(o.line, "%s: %s is neither a list nor a mapping", o.label, joinPath(o.path, o.key))
		}
	}
	for i, mt := range matchers {
		if !hit[i] {
			m.warnf(items.Content[i].Line, "%s: nothing matches %s", o.label, mt)
		}
	}
	if o.path == "" && (o.key == "proxies" || o.key == "proxy-groups") && len(removed) > 0 {
		names := make(map[string]bool, len(removed))
		for _, n := range removed {
			names[n] = true
		}
		removeFromGroups(m.root, names)
	}
	return nil
}

// removeFromGroups 把已删除的节点或策略组从所有策略组的 proxies 中移除。
func removeFromGroups(root *yaml.Node, names map[string]bool) {
	j := valueIndex(root, "proxy-groups")
	if j < 0 || root.Content[j].Kind != yaml.SequenceNode {
		return
	}
	for _, g := range root.Content[j].Content {
		if list := groupProxies(g); list != nil {
			list.Content = slices.DeleteFunc(list.Content, func(n *yaml.Node) bool {
				return n.Kind == yaml.ScalarNode && names[n.Value]
			})
		}
	}
}

// matcher 匹配名称：普通字符串精确匹配，/正则/ 按正则匹配（与 mihomo 相同，见 compileRegexp）。
type matcher struct {
	text string
	re   *regexp2.Regexp
}

func (m *mixer) matcher(op string, n *yaml.Node) (matcher, error) {
	if n.Kind != yaml.ScalarNode {
		return matcher{}, m.errorf(n.Line, "%s: expected a name or /regular expression/", op)
	}
	s := n.Value
	if len(s) >= 2 && strings.HasPrefix(s, "/") && strings.HasSuffix(s, "/") {
		re, err := compileRegexp(s[1 : len(s)-1])
		if err != nil {
			return matcher{}, m.errorf(n.Line, "%s: invalid regular expression %s: %v", op, s, err)
		}
		return matcher{text: s, re: re}, nil
	}
	return matcher{text: s}, nil
}

func (mt matcher) match(name string) bool {
	if mt.re != nil {
		return matchRegexp(mt.re, name)
	}
	return name == mt.text
}

func (mt matcher) String() string { return mt.text }

// itemName 返回列表项的名称：映射取 name 字段，标量取其值。
func itemName(n *yaml.Node) (string, bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value, true
	case yaml.MappingNode:
		if i := valueIndex(n, "name"); i >= 0 && n.Content[i].Kind == yaml.ScalarNode {
			return n.Content[i].Value, true
		}
	}
	return "", false
}

func isMatchRule(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(n.Value)), "MATCH,")
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// setValue 设置映射中 key 的值。键已存在时只替换值；否则用不带注释的新键节点追加到末尾。
func setValue(m *yaml.Node, key string, value *yaml.Node) {
	setPair(m, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// clone 返回 n 的深拷贝。同一个 mixin 节点可能插入多处（如正则匹配的 patch），
// 拷贝后各处互不影响。
func clone(n *yaml.Node) *yaml.Node {
	c := *n
	if n.Content != nil {
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			c.Content[i] = clone(child)
		}
	}
	return &c
}

// stripComments 去掉 mixin 中的注释：生成的配置只保留模板的注释，
// 不带入 mixin 示例中被注释掉的大段内容。
func stripComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		stripComments(c)
	}
}

// leafPaths 返回映射中所有非映射值的路径。
func leafPaths(m *yaml.Node, prefix []string) [][]string {
	var out [][]string
	for i := 0; i+1 < len(m.Content); i += 2 {
		p := append(slices.Clone(prefix), m.Content[i].Value)
		if v := m.Content[i+1]; v.Kind == yaml.MappingNode {
			out = append(out, leafPaths(v, p)...)
		} else {
			out = append(out, p)
		}
	}
	return out
}

// lookup 按路径查找映射中的值，不存在时返回 nil。
func lookup(m *yaml.Node, path []string) *yaml.Node {
	for _, k := range path {
		if m == nil || m.Kind != yaml.MappingNode {
			return nil
		}
		i := valueIndex(m, k)
		if i < 0 {
			return nil
		}
		m = m.Content[i]
	}
	return m
}

func sameNode(a, b *yaml.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind || a.Value != b.Value || len(a.Content) != len(b.Content) {
		return false
	}
	for i := range a.Content {
		if !sameNode(a.Content[i], b.Content[i]) {
			return false
		}
	}
	return true
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
