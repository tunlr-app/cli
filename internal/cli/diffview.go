package cli

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

const (
	// defaultDiffWidth 是不知道终端宽度时（输出不是终端）差异中一行最多显示的列数。
	defaultDiffWidth = 160
	// shortHead 是截短改动行时行首保留的列数，通常能看出是哪个策略组。
	shortHead = 24
	// shortAround 是截短改动行时改动处前后各保留的列数。
	shortAround = 24
)

// shortenDiff 截短统一格式差异中超过 width 列的行，供终端显示；--full-diff 和 --json 照旧给出整行。
// 订阅常把一个策略组写成一行（{name: …, proxies: [全部节点]}），这样的行出现在上下文里，或者
// 只改了其中几个节点时，整行照印会占满屏幕。
//   - 一段连续的删除行和紧随其后的新增行按顺序两两配对，去掉两行相同的开头和结尾，只保留行首和
//     改动处前后各一小段，其余写成 …；
//   - 其余的行（上下文、没有配对的改动行）只保留开头 width 列，末尾加 …。
//
// 返回截短后的差异，以及是否有行被截短。
func shortenDiff(diff string, width int) (string, bool) {
	lines := strings.SplitAfter(diff, "\n")
	shortened := false
	op := func(i int) byte {
		if lines[i] == "" {
			return 0
		}
		return lines[i][0]
	}
	single := func(i int) {
		body, nl := strings.CutSuffix(lines[i], "\n")
		if ansi.StringWidth(body) <= width {
			return
		}
		lines[i] = ansi.Truncate(body, width-2, "") + " …"
		if nl {
			lines[i] += "\n"
		}
		shortened = true
	}
	pair := func(i, j int) {
		a, anl := strings.CutSuffix(lines[i], "\n")
		b, bnl := strings.CutSuffix(lines[j], "\n")
		if ansi.StringWidth(a) <= width && ansi.StringWidth(b) <= width {
			return
		}
		ra, rb := []rune(a[1:]), []rune(b[1:])
		p := 0
		for p < len(ra) && p < len(rb) && ra[p] == rb[p] {
			p++
		}
		s := 0
		for s < len(ra)-p && s < len(rb)-p && ra[len(ra)-1-s] == rb[len(rb)-1-s] {
			s++
		}
		lines[i] = a[:1] + elide(ra, p, len(ra)-s, width-1)
		lines[j] = b[:1] + elide(rb, p, len(rb)-s, width-1)
		if anl {
			lines[i] += "\n"
		}
		if bnl {
			lines[j] += "\n"
		}
		shortened = true
	}
	// 前两行是文件头。
	for i := 2; i < len(lines); {
		if op(i) != '-' {
			if op(i) != '@' {
				single(i)
			}
			i++
			continue
		}
		j := i
		for j < len(lines) && op(j) == '-' {
			j++
		}
		k := j
		for k < len(lines) && op(k) == '+' {
			k++
		}
		for n := 0; n < max(j-i, k-j); n++ {
			switch {
			case n < j-i && n < k-j:
				pair(i+n, j+n)
			case n < j-i:
				single(i + n)
			default:
				single(j + n)
			}
		}
		i = k
	}
	return strings.Join(lines, ""), shortened
}

// elide 返回 r 的简写：r[from:to] 是与配对的另一行不同的部分，保留行首 shortHead 列和改动处前后
// 各 shortAround 列，省去的部分写成 …；改动本身超过 width 列时只保留开头的 width 列。
func elide(r []rune, from, to, width int) string {
	before, changed, after := string(r[:from]), string(r[from:to]), string(r[to:])
	// 截取处尽量落在列表项之间的“, ”上，不把节点名截成两半。
	var b strings.Builder
	if w := ansi.StringWidth(before); w <= shortHead+shortAround+3 {
		b.WriteString(before)
	} else {
		head := ansi.Truncate(before, shortHead, "")
		if i := strings.LastIndex(head, ", "); i > 0 {
			head = head[:i+1]
		}
		tail := ansi.TruncateLeft(before, w-shortAround, "")
		if i := strings.Index(tail, ", "); i >= 0 && i+2 < len(tail) {
			tail = tail[i+2:]
		}
		b.WriteString(head)
		b.WriteString(" … ")
		b.WriteString(tail)
	}
	if ansi.StringWidth(changed) > width {
		b.WriteString(ansi.Truncate(changed, width, ""))
		b.WriteString(" …")
		return b.String()
	}
	b.WriteString(changed)
	if ansi.StringWidth(after) <= shortAround+3 {
		b.WriteString(after)
	} else {
		tail := ansi.Truncate(after, shortAround, "")
		if i := strings.LastIndex(tail, ", "); i > 0 {
			tail = tail[:i+1]
		}
		b.WriteString(tail)
		b.WriteString(" …")
	}
	return b.String()
}
