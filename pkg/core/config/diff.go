package config

import (
	"fmt"
	"sort"
	"strings"
)

// diffContext 是 Diff 输出的每处改动前后保留的上下文行数，与 diff -u 一致。
const diffContext = 3

// diffLine 是逐行 diff 中的一行，op 为 ' '、'-' 或 '+'。
type diffLine struct {
	op   byte
	text string
}

// DiffSection 是差异中一个顶层键（段）增删的行数。
type DiffSection struct {
	// Key 是顶层键；第一个顶层键之前的行（如文件开头的注释）为空串。
	Key     string `json:"key"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// Diff 返回 a 到 b 的统一格式（diff -u）逐行差异，以及按顶层键汇总的增删行数（按出现顺序），
// 两者相同时返回空串和 nil。
//
// 差异用锚定算法（git 称为 patience diff）计算：以新旧两侧各只出现一次的行为锚点，取锚点的最长
// 公共子序列对齐，锚点之间的空隙递归处理。耗时为 O(n log n)，新旧差别很大时（如换了机场）也不会
// 像求最短差异那样耗时成倍增长；结果对配置这类每行多半唯一的文本也更易读，不会拿无关的
// 重复行（如各策略组里的 type: select）去对齐。
func Diff(oldName, newName string, a, b []byte) (string, []DiffSection) {
	if string(a) == string(b) {
		return "", nil
	}
	var ls []diffLine
	anchored(splitLines(string(a)), splitLines(string(b)), &ls)
	return unified(oldName, newName, ls), sections(ls)
}

// anchored 把 x 到 y 的逐行差异追加到 ls。
func anchored(x, y []string, ls *[]diffLine) {
	// 去掉相同的开头和结尾，空隙里原本不唯一的行因此常常变得唯一。
	p := 0
	for p < len(x) && p < len(y) && x[p] == y[p] {
		p++
	}
	s := 0
	for s < len(x)-p && s < len(y)-p && x[len(x)-1-s] == y[len(y)-1-s] {
		s++
	}
	emit := func(op byte, lines []string) {
		for _, l := range lines {
			*ls = append(*ls, diffLine{op, l})
		}
	}
	emit(' ', x[:p])
	mx, my := x[p:len(x)-s], y[p:len(y)-s]

	if matches := uniqueLCS(mx, my); len(matches) == 0 {
		// 没有可以对齐的行：整段删除再整段新增。
		emit('-', mx)
		emit('+', my)
	} else {
		var i, j int
		for _, m := range matches {
			anchored(mx[i:m.x], my[j:m.y], ls)
			emit(' ', mx[m.x:m.x+1])
			i, j = m.x+1, m.y+1
		}
		anchored(mx[i:], my[j:], ls)
	}
	emit(' ', x[len(x)-s:])
}

// linePair 是 x、y 中相同的一行的位置。
type linePair struct{ x, y int }

// uniqueLCS 返回 x、y 中各只出现一次的行（锚点）的最长公共子序列，按位置递增。
// 按 x 的顺序排列锚点在 y 中的位置，求其最长递增子序列（耐心排序，O(n log n)）。
func uniqueLCS(x, y []string) []linePair {
	if len(x) == 0 || len(y) == 0 {
		return nil
	}
	// count 记录每行在 x、y 中各出现几次（超过 1 次按 2 计）及在 y 中的位置。
	type count struct{ x, y, at int }
	counts := make(map[string]*count)
	for _, l := range x {
		c := counts[l]
		if c == nil {
			c = &count{}
			counts[l] = c
		}
		c.x = min(c.x+1, 2)
	}
	for i, l := range y {
		if c := counts[l]; c != nil {
			c.y = min(c.y+1, 2)
			c.at = i
		}
	}
	var anchors []linePair
	for i, l := range x {
		if c := counts[l]; c.x == 1 && c.y == 1 {
			anchors = append(anchors, linePair{i, c.at})
		}
	}
	if len(anchors) == 0 {
		return nil
	}

	// tails[k] 是长度为 k+1 的递增子序列中结尾最小者在 anchors 中的下标；prev 用于回溯。
	tails := make([]int, 0, len(anchors))
	prev := make([]int, len(anchors))
	for i, a := range anchors {
		k := sort.Search(len(tails), func(k int) bool { return anchors[tails[k]].y >= a.y })
		prev[i] = -1
		if k > 0 {
			prev[i] = tails[k-1]
		}
		if k == len(tails) {
			tails = append(tails, i)
		} else {
			tails[k] = i
		}
	}
	lcs := make([]linePair, len(tails))
	for k, i := len(tails)-1, tails[len(tails)-1]; k >= 0; k, i = k-1, prev[i] {
		lcs[k] = anchors[i]
	}
	return lcs
}

// sections 按顶层键汇总增删的行数。新旧两侧分别跟踪当前所在的段：删除的行属于旧配置中它所在的段，
// 新增的行属于新配置中它所在的段。
func sections(ls []diffLine) []DiffSection {
	var out []DiffSection
	index := map[string]int{}
	add := func(key string, added, removed int) {
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, DiffSection{Key: key})
		}
		out[i].Added += added
		out[i].Removed += removed
	}
	var oldKey, newKey string
	for _, l := range ls {
		if k, ok := topLevelKey(l.text); ok {
			if l.op != '+' {
				oldKey = k
			}
			if l.op != '-' {
				newKey = k
			}
		}
		switch l.op {
		case '-':
			add(oldKey, 0, 1)
		case '+':
			add(newKey, 1, 0)
		}
	}
	return out
}

// topLevelKey 返回顶层键所在行的键名。生成的配置中第 0 列的行只可能是顶层键或注释（见 spaceSections）。
func topLevelKey(line string) (string, bool) {
	if line == "" || line[0] == ' ' || line[0] == '#' {
		return "", false
	}
	k, _, ok := strings.Cut(line, ":")
	if !ok {
		return "", false
	}
	return strings.Trim(k, `"'`), true
}

// unified 把逐行差异写成统一格式。
func unified(oldName, newName string, ls []diffLine) string {
	// before[i] 是 ls[i] 之前旧、新两侧各有多少行，用于计算每段的起始行号。
	type counts struct{ old, new int }
	before := make([]counts, len(ls)+1)
	for i, l := range ls {
		before[i+1] = before[i]
		if l.op != '+' {
			before[i+1].old++
		}
		if l.op != '-' {
			before[i+1].new++
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	for i := 0; i < len(ls); {
		for i < len(ls) && ls[i].op == ' ' {
			i++
		}
		if i == len(ls) {
			break
		}
		// 两处改动之间的相同行不超过两倍上下文时并入同一段。
		end := i
		for j := i; j < len(ls); {
			if ls[j].op != ' ' {
				j++
				end = j
				continue
			}
			k := j
			for k < len(ls) && ls[k].op == ' ' {
				k++
			}
			if k == len(ls) || k-j > 2*diffContext {
				break
			}
			j = k
		}
		start, stop := max(i-diffContext, 0), min(end+diffContext, len(ls))
		oldN, newN := before[stop].old-before[start].old, before[stop].new-before[start].new
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(before[start].old, oldN), hunkRange(before[start].new, newN))
		for _, l := range ls[start:stop] {
			out.WriteByte(l.op)
			out.WriteString(l.text)
			out.WriteByte('\n')
		}
		i = stop
	}
	return out.String()
}

// hunkRange 按 diff -u 的写法给出一段的起始行号和行数：行数为 1 时省略，为 0 时起始行号指向前一行。
func hunkRange(before, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", before)
	case 1:
		return fmt.Sprintf("%d", before+1)
	}
	return fmt.Sprintf("%d,%d", before+1, n)
}

// splitLines 按行切分，去掉换行符；末尾的换行不产生空行。
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
