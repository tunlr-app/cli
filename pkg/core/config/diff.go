package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// diffContext 是 Diff 输出的每处改动前后保留的上下文行数，与 diff -u 一致。
const diffContext = 3

// diffLine 是逐行 diff 中的一行，op 为 ' '、'-' 或 '+'。
type diffLine struct {
	op   byte
	text string
}

// diffTimeout 是计算差异的时限。新旧配置差别很大时（如换了机场）逐行比较可能要几秒；超时后得到的
// 差异仍然正确，只是不一定最短，这种情况下差异本来就大，用户通常只看按段的汇总。
const diffTimeout = time.Second

// DiffSection 是差异中一个顶层键（段）增删的行数。
type DiffSection struct {
	// Key 是顶层键；第一个顶层键之前的行（如文件开头的注释）为空串。
	Key     string `json:"key"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// Diff 返回 a 到 b 的统一格式（diff -u）逐行差异，以及按顶层键汇总的增删行数（按出现顺序），
// 两者相同时返回空串和 nil。
func Diff(oldName, newName string, a, b []byte) (string, []DiffSection) {
	if string(a) == string(b) {
		return "", nil
	}
	dmp := diffmatchpatch.New()
	dmp.DiffTimeout = diffTimeout
	ra, rb, lines := dmp.DiffLinesToRunes(string(a), string(b))
	diffs := dmp.DiffCharsToLines(dmp.DiffMainRunes(ra, rb, false), lines)

	var ls []diffLine
	for _, d := range diffs {
		op := byte(' ')
		switch d.Type {
		case diffmatchpatch.DiffDelete:
			op = '-'
		case diffmatchpatch.DiffInsert:
			op = '+'
		}
		for _, t := range splitLines(d.Text) {
			ls = append(ls, diffLine{op, t})
		}
	}
	return unified(oldName, newName, ls), sections(ls)
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
