package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDrift(t *testing.T) {
	p, _, _ := newPipeline(t)
	ctx := context.Background()
	drift := func() *Drift {
		t.Helper()
		d, err := p.Drift(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	key := func() string {
		t.Helper()
		k, err := p.DriftKey()
		if err != nil {
			t.Fatal(err)
		}
		return k
	}

	// 没有发布过：没有漂移可言。
	if d := drift(); d != nil {
		t.Fatalf("unpublished: %+v", d)
	}
	mustApply(t, p, mustPlan(t, p))
	if d := drift(); d == nil || d.State != DriftNone || len(d.Sections) != 0 {
		t.Fatalf("published: %+v", d)
	}

	// 改了 mixin、还没发布：指纹变化，列出有变化的顶层键。
	before := key()
	mixin := filepath.Join(p.Layout.MixinDir(), "10-log.yaml")
	writeFile(t, mixin, "log-level: warning\n")
	if key() == before {
		t.Error("key unchanged after adding a mixin")
	}
	if d := drift(); d.State != DriftPending || !slices.Equal(d.Sections, []string{"log-level"}) {
		t.Errorf("pending: %+v", d)
	}

	// 合成出错：记为 DriftError，不是错误。
	writeFile(t, mixin, "tun:\n  enable: true\n")
	if d := drift(); d.State != DriftError || d.Error == "" {
		t.Errorf("error: %+v", d)
	}
	if err := os.Remove(mixin); err != nil {
		t.Fatal(err)
	}

	// 生效的配置在 tunlr 之外被改过。
	cfg, err := os.ReadFile(p.Layout.CoreConfig())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.Layout.CoreConfig(), string(cfg)+"\nipv6: false\n")
	if d := drift(); d.State != DriftEdited || len(d.Sections) == 0 {
		t.Errorf("edited: %+v", d)
	}
}
