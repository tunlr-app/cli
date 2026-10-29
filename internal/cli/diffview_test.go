package cli

import (
	"fmt"
	"strings"
	"testing"
)

func TestShortenDiff(t *testing.T) {
	var names []string
	for i := range 40 {
		names = append(names, fmt.Sprintf("'🇭🇰 香港 %02d'", i))
	}
	group := func(skip int) string {
		var keep []string
		for i, n := range names {
			if i != skip {
				keep = append(keep, n)
			}
		}
		return "  - {name: 节点选择, type: select, proxies: [DIRECT, " + strings.Join(keep, ", ") + "]}"
	}
	diff := "--- current\n+++ new\n@@ -1,4 +1,4 @@\n" +
		" " + group(-1) + "\n" +
		"-" + group(-1) + "\n" +
		"+" + group(20) + "\n" +
		"-  - short removed\n" +
		" rules:\n"
	got, shortened := shortenDiff(diff, 100)
	want := "--- current\n+++ new\n@@ -1,4 +1,4 @@\n" +
		"   - {name: 节点选择, type: select, proxies: [DIRECT, '🇭🇰 香港 00', '🇭🇰 香港 01', '🇭🇰 香港 02', ' …\n" +
		"-  - {name: 节点选择, … '🇭🇰 香港 20', '🇭🇰 香港 21', '🇭🇰 香港 22', …\n" +
		"+  - {name: 节点选择, … '🇭🇰 香港 21', '🇭🇰 香港 22', …\n" +
		"-  - short removed\n" +
		" rules:\n"
	if !shortened {
		t.Error("not shortened")
	}
	if got != want {
		t.Errorf("shortenDiff =\n%s\nwant\n%s", got, want)
	}

	short := "--- a\n+++ b\n@@ -1 +1 @@\n-a: 1\n+a: 2\n"
	if got, shortened := shortenDiff(short, 100); got != short || shortened {
		t.Errorf("short diff changed: %q", got)
	}
}
