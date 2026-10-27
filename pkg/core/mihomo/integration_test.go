package mihomo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tunlr-app/cli/pkg/core"
	"github.com/tunlr-app/cli/pkg/core/config"
	"github.com/tunlr-app/cli/pkg/core/download"
	"github.com/tunlr-app/cli/templates"
)

// 集成测试使用真实的 mihomo 与 GitHub，只在设置了 TUNLR_TEST_MIHOMO（内核路径）时运行，
// 例如在测试机上：TUNLR_TEST_MIHOMO=/var/tmp/tunlr-dev/mihomo TUNLR_TEST_GITHUB_MIRROR=https://gh-proxy.com/
func realValidator(t *testing.T) *Validator {
	bin := os.Getenv("TUNLR_TEST_MIHOMO")
	if bin == "" {
		t.Skip("TUNLR_TEST_MIHOMO not set")
	}
	return &Validator{
		Binary:   bin,
		HomeDir:  t.TempDir(),
		Download: &download.Client{Mirror: os.Getenv("TUNLR_TEST_GITHUB_MIRROR")},
	}
}

const integrationSubscription = `port: 7890
external-controller: 0.0.0.0:9090
proxies:
  - {name: a, type: ss, server: a.example.net, port: 1, cipher: aes-128-gcm, password: p}
proxy-groups:
  - {name: g, type: select, proxies: [a, DIRECT]}
rules:
  - GEOSITE,cn,DIRECT
  - GEOIP,CN,DIRECT
  - MATCH,g
`

func TestIntegrationValidate(t *testing.T) {
	v := realValidator(t)
	ctx := context.Background()

	res, err := config.Compose(config.Layers{Template: templates.Config, Subscription: []byte(integrationSubscription)})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(ctx, res.Config, nil); err != nil {
		t.Fatalf("composed config: %v", err)
	}
	// 只下载了需要的 geo 文件，-t 没有往工作目录写其他东西。
	entries, err := os.ReadDir(v.HomeDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "GeoSite.dat,geoip.metadb" {
		t.Errorf("home contains %v", names)
	}

	bad := strings.Replace(string(res.Config), "MATCH,g", "MATCH,NOPE", 1)
	err = v.Validate(ctx, []byte(bad), nil)
	var ce *CheckError
	if !errors.As(err, &ce) || !errors.Is(err, core.ErrCheckFailed) || ce.MissingData {
		t.Fatalf("bad config: err = %v", err)
	}
	if !strings.Contains(err.Error(), "proxy [NOPE] not found") {
		t.Errorf("err = %v", err)
	}
}

func TestIntegrationMissingData(t *testing.T) {
	v := realValidator(t)
	v.Download = nil
	// 让内核现场下载的地址立即失败，避免等待直连 GitHub 超时。
	cfg := "geox-url:\n  mmdb: http://127.0.0.1:1/geoip.metadb\n" +
		"rules:\n  - GEOIP,CN,DIRECT\n  - MATCH,DIRECT\n"
	err := v.Validate(context.Background(), []byte(cfg), nil)
	var ce *CheckError
	if !errors.As(err, &ce) || !ce.MissingData || errors.Is(err, core.ErrCheckFailed) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(v.HomeDir, "geoip.metadb")); err == nil {
		t.Error("validation wrote into the home directory")
	}
}
