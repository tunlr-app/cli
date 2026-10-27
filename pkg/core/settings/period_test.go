package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestGeoUpdateSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	s, err := Load(path)
	if err != nil || s.GeoPeriod() != DefaultGeoUpdate || s.GeoUpdaterValue() != GeoUpdaterTunlr {
		t.Fatalf("defaults: %+v, %v", s, err)
	}
	if on, _ := s.GeoAutoUpdate(); on {
		t.Error("geo-auto-update on by default")
	}
	if err := Set(path, KeyGeoUpdate, "12x"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("invalid period saved: %v", err)
	}
	if err := Set(path, KeyGeoUpdater, "clash"); !errors.Is(err, core.ErrInvalidArgument) {
		t.Errorf("invalid updater saved: %v", err)
	}
	if err := Set(path, KeyGeoUpdate, "2d"); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, KeyGeoUpdater, GeoUpdaterMihomo); err != nil {
		t.Fatal(err)
	}
	s, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if on, hours := s.GeoAutoUpdate(); !on || hours != 48 {
		t.Errorf("mihomo updater: on %v, %d hours", on, hours)
	}
	if err := Set(path, KeyGeoUpdate, "off"); err != nil {
		t.Fatal(err)
	}
	if s, _ = Load(path); s == nil {
		t.Fatal("load")
	} else if on, _ := s.GeoAutoUpdate(); on {
		t.Error("off with the mihomo updater still turns geo-auto-update on")
	}
	// 删光键再写入时仍是一行一个键，不是 "{geo-update: 2d}"。
	if err := Set(path, KeyGeoUpdate, ""); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, KeyGeoUpdater, ""); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, KeyGeoUpdate, "2d"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "\ngeo-update: 2d\n") && !strings.HasPrefix(string(data), "geo-update: 2d\n") {
		t.Errorf("after deleting every key, the file is written as %q", data)
	}
	if err := os.WriteFile(path, []byte("geo-update: 2w\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("a hand-written invalid period was accepted")
	}
}
