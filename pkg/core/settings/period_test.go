package settings

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tunlr-app/cli/pkg/core"
)

func TestParsePeriod(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"1h": time.Hour, "6h": 6 * time.Hour, "1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "365d": 365 * 24 * time.Hour,
		"off": 0,
	} {
		if got, err := ParsePeriod(in); err != nil || got != want {
			t.Errorf("ParsePeriod(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "h", "0h", "-1d", "+1d", "1", "1m", "1w", "1.5d", " 1d", "366d", "9000h", "Off"} {
		if _, err := ParsePeriod(in); !errors.Is(err, core.ErrInvalidArgument) {
			t.Errorf("ParsePeriod(%q) = %v, want an invalid-argument error", in, err)
		}
	}
}

func TestDescribePeriod(t *testing.T) {
	for in, want := range map[string]string{
		"1h": "hourly", "6h": "every 6 hours", "24h": "daily", "1d": "daily", "3d": "every 3 days", "7d": "weekly", "off": "never",
	} {
		if got := DescribePeriod(in); got != want {
			t.Errorf("DescribePeriod(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetAfterDeletingEveryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := Set(path, "user-agent", "x"); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, "user-agent", ""); err != nil {
		t.Fatal(err)
	}
	// 删光键再写入时仍是一行一个键，不是 "{user-agent: y}"。
	if err := Set(path, "user-agent", "y"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "user-agent: y\n" {
		t.Errorf("after deleting every key, the file is written as %q", data)
	}
}
