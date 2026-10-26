package config

import (
	"fmt"
	"strings"
	"testing"
)

func numbered(n int, change map[int]string) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if s, ok := change[i]; ok {
			if s != "" {
				b.WriteString(s + "\n")
			}
			continue
		}
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return []byte(b.String())
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name string
		a, b []byte
		want string
	}{
		{"identical", numbered(5, nil), numbered(5, nil), ""},
		{
			"one change in the middle",
			numbered(20, nil), numbered(20, map[int]string{10: "changed"}),
			"@@ -7,7 +7,7 @@\n line 7\n line 8\n line 9\n-line 10\n+changed\n line 11\n line 12\n line 13\n",
		},
		{
			// 两处改动之间相隔 6 行（两倍上下文），并为一段。
			"nearby changes merge",
			numbered(20, nil), numbered(20, map[int]string{5: "x", 12: "y"}),
			"@@ -2,14 +2,14 @@\n line 2\n line 3\n line 4\n-line 5\n+x\n line 6\n line 7\n line 8\n line 9\n line 10\n line 11\n-line 12\n+y\n line 13\n line 14\n line 15\n",
		},
		{
			"distant changes split",
			numbered(20, nil), numbered(20, map[int]string{2: "x", 18: "y"}),
			"@@ -1,5 +1,5 @@\n line 1\n-line 2\n+x\n line 3\n line 4\n line 5\n@@ -15,6 +15,6 @@\n line 15\n line 16\n line 17\n-line 18\n+y\n line 19\n line 20\n",
		},
		{
			"deletion at start",
			numbered(5, nil), numbered(5, map[int]string{1: ""}),
			"@@ -1,4 +1,3 @@\n-line 1\n line 2\n line 3\n line 4\n",
		},
		{
			"from empty",
			nil, []byte("a\nb\n"),
			"@@ -0,0 +1,2 @@\n+a\n+b\n",
		},
		{
			"single line",
			[]byte("a\n"), []byte("b\n"),
			"@@ -1 +1 @@\n-a\n+b\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Diff("old", "new", tt.a, tt.b)
			want := tt.want
			if want != "" {
				want = "--- old\n+++ new\n" + want
			}
			if got != want {
				t.Errorf("got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}
