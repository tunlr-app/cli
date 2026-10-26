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

	"go.yaml.in/yaml/v3"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/fsutil"
)

// Mixin 是一份用户 mixin 文件。
//
// mixin 的写法与 mihomo 配置相同：映射逐层合并，标量和列表整体替换，值为 null 时删除该键。
// 另有四种前缀操作，可用在任何一层映射中：
//
//	prepend-<键> / append-<键>  在列表前插入 / 后追加；rules 和 sub-rules 的列表追加在末尾的 MATCH 之前
//	patch-<键>                 按 name 找到列表中的项，用同样的语义修改它
//	delete-<键>                按 name（字符串列表按值，映射按键）删除；顶层删除节点或策略组时，
//	                           同时从所有策略组的 proxies 中移除
//
// name 写成 /正则/ 时匹配所有符合的项。同一层中按 替换 → 删除 → 修改 → 插入 的顺序执行，
// 与键在文件中的顺序无关。顶层以 x- 开头的键不写入配置，用来放 YAML 锚点。
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

// MixinFileName 把用户给出的名称（可带或不带 .yaml）转换为 mixin 文件名。
func MixinFileName(name string) (string, error) {
	stem := strings.TrimSuffix(name, mixinExt)
	if !mixinName.MatchString(stem) {
		return "", fmt.Errorf("%w: invalid mixin name %q: use letters, digits, '.', '_' and '-' (e.g. 20-rules)", core.ErrInvalidArgument, name)
	}
	return stem + mixinExt, nil
}

// LoadMixins 读取目录中的 mixin 文件，按文件名排序。只读取扩展名为 .yaml 的文件；
// 以点开头的文件（如编辑器的临时文件）和子目录被忽略，子目录预留给将来按订阅区分的 mixin。
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

// ErrMixinChanged 表示 mixin 文件在编辑期间被别人改动过。
var ErrMixinChanged = errors.New("the mixin file was changed by someone else while you were editing it")

// SaveMixin 原子写入 mixin 文件。previous 和 existed 是开始编辑时读到的内容和文件是否存在；
// 文件此后被改动过时返回 ErrMixinChanged，不覆盖别人的修改。
// mixin 可能含有节点密码，目录为 0700，文件为 0600。
func SaveMixin(dir string, m Mixin, previous []byte, existed bool) error {
	path := filepath.Join(dir, m.Name)
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
	return fsutil.WriteFile(path, m.Data, 0o600)
}

// 前缀操作，按执行顺序排列。
type mixinOp int

const (
	opDelete mixinOp = iota
	opPatch
	opInsert // prepend 与 append
)

// mixer 把 mixin 文件逐个应用到配置上，记录警告和文件之间的覆盖。
type mixer struct {
	root *yaml.Node
	// file 是正在应用的文件。
	file string
	// subKeys 是来自订阅的顶层键。
	subKeys map[string]bool
	// setBy 记录普通键的路径最近由哪个文件设置。
	setBy     map[string]string
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
}

// apply 把 mixin 映射 src 应用到配置映射 dst 上。path 是 dst 在配置中的路径，顶层为空。
func (m *mixer) apply(dst, src *yaml.Node, path string) error {
	type pending struct {
		op      mixinOp
		prepend bool
		key     string
		keyNode *yaml.Node
		value   *yaml.Node
	}
	var ops []pending
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		if path == "" && strings.HasPrefix(k.Value, "x-") {
			continue
		}
		op, prepend, key, ok := parseOp(k.Value)
		if !ok {
			if err := m.set(dst, k, v, path); err != nil {
				return err
			}
			continue
		}
		if key == "" {
			return m.errorf(k.Line, "%s: missing the key to operate on", k.Value)
		}
		ops = append(ops, pending{op, prepend, key, k, v})
	}
	slices.SortStableFunc(ops, func(a, b pending) int { return int(a.op) - int(b.op) })
	for _, o := range ops {
		if isNull(o.value) {
			continue
		}
		if o.value.Kind != yaml.SequenceNode {
			return m.errorf(o.keyNode.Line, "%s: value must be a list", o.keyNode.Value)
		}
		var err error
		switch o.op {
		case opDelete:
			err = m.delete(dst, o.keyNode, o.key, o.value, path)
		case opPatch:
			err = m.patch(dst, o.keyNode, o.key, o.value, path)
		case opInsert:
			err = m.insert(dst, o.keyNode, o.key, o.value, path, o.prepend)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// parseOp 识别前缀操作，返回操作、是否为 prepend 和被操作的键。
func parseOp(k string) (op mixinOp, prepend bool, key string, ok bool) {
	for _, p := range []struct {
		prefix  string
		op      mixinOp
		prepend bool
	}{
		{"prepend-", opInsert, true},
		{"append-", opInsert, false},
		{"patch-", opPatch, false},
		{"delete-", opDelete, false},
	} {
		if key, ok := strings.CutPrefix(k, p.prefix); ok {
			return p.op, p.prepend, key, true
		}
	}
	return 0, false, "", false
}

// set 处理普通键：null 删除，映射逐层合并，其他值整体替换。
func (m *mixer) set(dst, k, v *yaml.Node, path string) error {
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
			// 从空映射开始应用，使新映射中的前缀操作和 null 同样生效。
			setValue(dst, k.Value, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
			j = valueIndex(dst, k.Value)
		}
		return m.apply(dst.Content[j], v, p)
	}
	if path == "" && j >= 0 && m.subKeys[k.Value] && v.Kind == yaml.SequenceNode {
		m.warnf(k.Line, "%s replaces the subscription's %s entirely; use prepend-%s, append-%s, patch-%s or delete-%s to keep them",
			k.Value, k.Value, k.Value, k.Value, k.Value, k.Value)
	}
	m.record(p)
	setValue(dst, k.Value, clone(v))
	return nil
}

// insert 处理 prepend-<键> 与 append-<键>，列表不存在时创建。
func (m *mixer) insert(dst, k *yaml.Node, key string, items *yaml.Node, path string, prepend bool) error {
	j := valueIndex(dst, key)
	if j < 0 {
		setValue(dst, key, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
		j = valueIndex(dst, key)
	}
	target := dst.Content[j]
	if target.Kind != yaml.SequenceNode {
		return m.errorf(k.Line, "%s: %s is not a list", k.Value, joinPath(path, key))
	}
	add := make([]*yaml.Node, len(items.Content))
	for i, it := range items.Content {
		add[i] = clone(it)
	}
	at := 0
	if !prepend {
		at = len(target.Content)
		// 规则在 MATCH 之后永远不会命中，追加的规则放在末尾的 MATCH 之前。
		isRules := (path == "" && key == "rules") || path == "sub-rules"
		if isRules && at > 0 && isMatchRule(target.Content[at-1]) {
			at--
		}
	}
	target.Content = slices.Insert(target.Content, at, add...)
	return nil
}

// patch 处理 patch-<键>：每一项按 name 匹配列表中的项，把其余字段应用到匹配的每一项上。
func (m *mixer) patch(dst, k *yaml.Node, key string, items *yaml.Node, path string) error {
	var target *yaml.Node
	if j := valueIndex(dst, key); j >= 0 {
		target = dst.Content[j]
		if target.Kind != yaml.SequenceNode {
			return m.errorf(k.Line, "%s: %s is not a list", k.Value, joinPath(path, key))
		}
	}
	for _, it := range items.Content {
		ni := -1
		if it.Kind == yaml.MappingNode {
			ni = valueIndex(it, "name")
		}
		if ni < 0 || it.Content[ni].Kind != yaml.ScalarNode {
			return m.errorf(it.Line, "%s: each item must be a mapping with a name", k.Value)
		}
		match, err := m.matcher(k.Value, it.Content[ni])
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
					if err := m.apply(t, body, fmt.Sprintf("%s[%s]", joinPath(path, key), name)); err != nil {
						return err
					}
				}
			}
		}
		if !matched {
			m.warnf(it.Line, "%s: no item named %s; nothing changed", k.Value, match)
		}
	}
	return nil
}

// delete 处理 delete-<键>：列表按 name（字符串按值）删除，映射按键删除。
func (m *mixer) delete(dst, k *yaml.Node, key string, items *yaml.Node, path string) error {
	matchers := make([]matcher, len(items.Content))
	for i, it := range items.Content {
		var err error
		if matchers[i], err = m.matcher(k.Value, it); err != nil {
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
	if j := valueIndex(dst, key); j >= 0 {
		switch target := dst.Content[j]; target.Kind {
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
			return m.errorf(k.Line, "%s: %s is neither a list nor a mapping", k.Value, joinPath(path, key))
		}
	}
	for i, mt := range matchers {
		if !hit[i] {
			m.warnf(items.Content[i].Line, "%s: nothing matches %s", k.Value, mt)
		}
	}
	if path == "" && (key == "proxies" || key == "proxy-groups") && len(removed) > 0 {
		m.removeFromGroups(removed)
	}
	return nil
}

// removeFromGroups 把已删除的节点或策略组从所有策略组的 proxies 中移除。
func (m *mixer) removeFromGroups(names []string) {
	j := valueIndex(m.root, "proxy-groups")
	if j < 0 || m.root.Content[j].Kind != yaml.SequenceNode {
		return
	}
	for _, g := range m.root.Content[j].Content {
		if g.Kind != yaml.MappingNode {
			continue
		}
		if pi := valueIndex(g, "proxies"); pi >= 0 && g.Content[pi].Kind == yaml.SequenceNode {
			list := g.Content[pi]
			list.Content = slices.DeleteFunc(list.Content, func(n *yaml.Node) bool {
				return n.Kind == yaml.ScalarNode && slices.Contains(names, n.Value)
			})
		}
	}
}

// matcher 匹配名称：普通字符串精确匹配，/正则/ 按正则匹配。
type matcher struct {
	text string
	re   *regexp.Regexp
}

func (m *mixer) matcher(op string, n *yaml.Node) (matcher, error) {
	if n.Kind != yaml.ScalarNode {
		return matcher{}, m.errorf(n.Line, "%s: expected a name or /regular expression/", op)
	}
	s := n.Value
	if len(s) >= 2 && strings.HasPrefix(s, "/") && strings.HasSuffix(s, "/") {
		re, err := regexp.Compile(s[1 : len(s)-1])
		if err != nil {
			return matcher{}, m.errorf(n.Line, "%s: invalid regular expression %s: %v", op, s, err)
		}
		return matcher{text: s, re: re}, nil
	}
	return matcher{text: s}, nil
}

func (mt matcher) match(name string) bool {
	if mt.re != nil {
		return mt.re.MatchString(name)
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

// keyLine 返回 mixin 中路径首个键所在的行，找不到时为 0。
func keyLine(m *yaml.Node, path []string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == path[0] {
			return m.Content[i].Line
		}
	}
	return 0
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

// lint 检查最终配置中有风险的设置。
func lint(root *yaml.Node) []Warning {
	var ws []Warning
	str := func(key string) string {
		if n := lookup(root, []string{key}); n != nil && n.Kind == yaml.ScalarNode && !isNull(n) {
			return n.Value
		}
		return ""
	}
	if ctl := str("external-controller"); ctl != "" {
		if host, _, err := net.SplitHostPort(ctl); err != nil || !isLoopback(host) {
			ws = append(ws, Warning{Message: fmt.Sprintf("external-controller listens on %s, which other hosts can reach; keep it on 127.0.0.1 and use an SSH tunnel", ctl)})
		}
		if str("secret") == "" {
			ws = append(ws, Warning{Message: "secret is empty: anyone who can reach external-controller can control mihomo"})
		}
	}
	if str("allow-lan") == "true" {
		if auth := lookup(root, []string{"authentication"}); auth == nil || len(auth.Content) == 0 {
			ws = append(ws, Warning{Message: "allow-lan is on without authentication: the proxy ports are open to the network"})
		}
	}
	return ws
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
