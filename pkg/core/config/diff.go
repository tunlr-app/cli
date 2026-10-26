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

// Diff 返回 a 到 b 的统一格式（diff -u）逐行差异，两者相同时返回空串。
func Diff(oldName, newName string, a, b []byte) string {
	if string(a) == string(b) {
		return ""
	}
	dmp := diffmatchpatch.New()
	// 超时后得到的差异仍然正确，只是不一定最短。
	dmp.DiffTimeout = 5 * time.Second
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
