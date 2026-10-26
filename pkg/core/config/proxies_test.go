package config

import (
	"reflect"
	"testing"
)

func TestDiffProxies(t *testing.T) {
	old := []byte(`proxies:
  - {name: '剩余流量：90.57 GB', type: trojan, server: a.example, port: 1, password: p}
  - {name: hk-1, type: trojan, server: a.example, port: 10001, password: p}
  - {name: hk-2, type: trojan, server: a.example, port: 10002, password: p}
  - {name: jp-1, type: trojan, server: b.example, port: 443, password: p}
  - {name: old, type: ss, server: c.example, port: 8388, cipher: aes-128-gcm, password: p}
`)
	new := []byte(`proxies:
  - {name: '剩余流量：90.55 GB', type: trojan, server: a.example, port: 2, password: p}
  - {name: hk-1, type: trojan, server: a.example, port: 20001, password: p}
  - {name: hk-2, type: trojan, server: a.example, port: 20002, password: q}
  - {name: jp-1, type: trojan, server: b.example, port: 443, password: p}
  - {name: new, type: ss, server: d.example, port: 8388, cipher: aes-128-gcm, password: p}
`)
	got := DiffProxies(old, new)
	// 名称和端口同时变化的信息节点列为一增一删。
	want := &ProxyChanges{
		Added:   []string{"剩余流量：90.55 GB", "new"},
		Removed: []string{"剩余流量：90.57 GB", "old"},
		Changed: 2,
		Fields:  map[string]int{"port": 2, "password": 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiffProxies = %+v\nwant %+v", got, want)
	}

	// 除名称外内容相同的是改名，与位置无关；位置相同而内容不同的不是。
	moved := []byte(`proxies:
  - {name: jp-1, type: trojan, server: b.example, port: 443, password: p}
  - {name: jp-one, type: trojan, server: b.example, port: 443, password: p}
  - {name: x, type: trojan, server: e.example, port: 443, password: p}
`)
	got = DiffProxies([]byte(`proxies:
  - {name: jp-1, type: trojan, server: b.example, port: 443, password: p}
  - {name: y, type: trojan, server: f.example, port: 443, password: p}
  - {name: jp-2, type: trojan, server: b.example, port: 443, password: p}
`), moved)
	want = &ProxyChanges{
		Added:   []string{"x"},
		Removed: []string{"y"},
		Renamed: []Rename{{From: "jp-2", To: "jp-one"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("moved: DiffProxies = %+v\nwant %+v", got, want)
	}

	if c := DiffProxies(old, old); c == nil || !c.Empty() {
		t.Errorf("same proxies = %+v", c)
	}
	if c := DiffProxies([]byte("mode: rule\n"), new); c == nil || len(c.Added) != 5 {
		t.Errorf("no proxies before = %+v", c)
	}
	if c := DiffProxies([]byte("proxies: [\n"), new); c != nil {
		t.Errorf("unparsable = %+v", c)
	}
}
