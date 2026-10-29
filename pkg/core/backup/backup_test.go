package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/state"
)

func sample() *Backup {
	return &Backup{
		Version: Version, Tunlr: "v1.0.0", Host: "box", CreatedAt: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
		Subscriptions: []state.Subscription{
			{Name: "main", URL: "https://example.com/sub?token=x"},
			{Name: "spare", URL: "https://example.org/sub"},
			{Name: "local", Path: "/root/airport.yaml"},
			{Name: "piped", Path: "-"},
		},
		Active:    "main",
		APISecret: "0123456789abcdef0123456789abcdef",
		Devices:   []state.Device{{IP: "192.168.1.5", Name: "tv", Route: state.RouteDirect}},
		Filter:    state.Filter{QUIC: true},
		Switches:  Switches{Tun: true, Gateway: "ens3", PanelLAN: true},
		Settings:  []byte("geo-update: 1d\n"),
		Mixins: []config.Mixin{{Name: "10-a.yaml", Data: []byte("a: 1\n")}, {Name: "20-b.yaml", Data: []byte("b: 2\n")},
			{Name: "main/10-filter.yaml", Data: []byte("c: 3\n")}},
		Contents: map[string][]byte{"main": []byte("proxies: []\n")},
	}
}

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := sample()
	if err := Write(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// archive 用给定的文件构造备份归档，backup.json 取自 b。
func archive(t *testing.T, b *Backup, extra map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, b); err != nil {
		t.Fatal(err)
	}
	if len(extra) == 0 {
		return &buf
	}
	// 重新打包，追加文件。
	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		data := new(bytes.Buffer)
		data.ReadFrom(tr)
		tw.WriteHeader(h)
		tw.Write(data.Bytes())
	}
	for name, content := range extra {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg})
		tw.Write([]byte(content))
	}
	tw.Close()
	zw.Close()
	return &out
}

func TestReadRejects(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(b *Backup)
		extra map[string]string
		want  string
	}{
		{"unknown file", nil, map[string]string{"etc/passwd": "x"}, `unexpected file "etc/passwd"`},
		{"traversal", nil, map[string]string{"mixin.d/../../x.yaml": "x"}, "invalid mixin file name"},
		{"unknown subscription", nil, map[string]string{"subscriptions/other.yaml": "x"}, `content for unknown subscription "other"`},
		{"bad settings", func(b *Backup) { b.Settings = []byte("nope: 1\n") }, nil, "settings:"},
		{"active without content", func(b *Backup) { b.Contents = nil }, nil, `the active subscription "main" has no content`},
		{"active missing", func(b *Backup) { b.Active = "gone"; b.Contents = nil }, nil, `the active subscription "gone" is not in the backup`},
		{"bad name", func(b *Backup) { b.Subscriptions[1].Name = "../x" }, nil, "subscription name"},
		{"bad url", func(b *Backup) { b.Subscriptions[1].URL = "file:///etc/passwd" }, nil, `subscription "spare": invalid URL`},
		{"relative path", func(b *Backup) { b.Subscriptions[2].Path = "airport.yaml" }, nil, `subscription "local": invalid source`},
		{"url and path", func(b *Backup) { b.Subscriptions[2].URL = "https://x.example/" }, nil, `subscription "local": invalid source`},
		{"nested mixin", nil, map[string]string{"mixin.d/main/x/1.yaml": "x"}, "invalid mixin file name"},
		{"newer", func(b *Backup) { b.Version = Version + 1 }, nil, "newer than this tunlr supports"},
		{"no version", func(b *Backup) { b.Version = 0 }, nil, "invalid version 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := sample()
			if tt.edit != nil {
				tt.edit(b)
			}
			_, err := Read(archive(t, b, tt.extra))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if !errors.Is(err, core.ErrInvalidArgument) {
				t.Errorf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}

	if _, err := Read(strings.NewReader("not gzip")); !errors.Is(err, ErrInvalid) {
		t.Errorf("not gzip: err = %v", err)
	}
	// 凭据不出现在错误信息中。
	b := sample()
	b.Subscriptions[0].URL = "ftp://secret-token@example.com/"
	if _, err := Read(archive(t, b, nil)); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("bad url error leaks the URL: %v", err)
	}
}
