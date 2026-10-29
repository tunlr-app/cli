package config

import (
	"reflect"
	"testing"
	"time"
)

func TestDetectGeo(t *testing.T) {
	tests := []struct {
		name, cfg string
		want      GeoNeeds
	}{
		{"none", "rules:\n  - DOMAIN-SUFFIX,cn,DIRECT\n  - MATCH,DIRECT\n", GeoNeeds{}},
		{"geox-url read, not a need", "geox-url:\n  geoip: https://x/geoip.dat\n  geosite: https://x/geosite.dat\n  asn: https://x/GeoLite2-ASN.mmdb\ngeo-auto-update: true\n",
			GeoNeeds{URLs: map[string]string{"geoip": "https://x/geoip.dat", "geosite": "https://x/geosite.dat", "asn": "https://x/GeoLite2-ASN.mmdb"}}},
		{"geoip rule", "rules:\n  - GEOIP,CN,DIRECT\n", GeoNeeds{GeoIP: true}},
		{"src-geoip in logic rule", "rules:\n  - AND,((SRC-GEOIP,CN),(NETWORK,UDP)),DIRECT\n", GeoNeeds{GeoIP: true}},
		{"geosite in dns", "dns:\n  nameserver-policy:\n    \"geosite:cn\": 223.5.5.5\n", GeoNeeds{GeoSite: true}},
		{"fallback-filter", "dns:\n  fallback-filter:\n    geoip: true\n", GeoNeeds{GeoIP: true}},
		{"asn", "rules:\n  - IP-ASN,13335,DIRECT\n", GeoNeeds{ASN: true}},
		{"sub-rules", "sub-rules:\n  s:\n    - GEOSITE,google,PROXY\n", GeoNeeds{GeoSite: true}},
		{"geodata mode", "geodata-mode: true\nrules:\n  - GEOIP,CN,DIRECT\n", GeoNeeds{GeoIP: true, GeodataMode: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DetectGeo([]byte(tt.cfg))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGeoSettings(t *testing.T) {
	cfg := "mode: rule\n" +
		"geodata-mode: true\n" +
		"geo-auto-update: True\n" +
		"geo-update-interval: 168\n" +
		"geox-url:\n  geoip: https://m.example/https://github.com/a/b/releases/download/v1/geoip.dat\n" +
		"rules:\n  - GEOIP,CN,DIRECT\n" +
		"dns:\n  enable: true\n"
	needs, update, err := GeoSettings([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	want := GeoNeeds{GeodataMode: true, URLs: map[string]string{"geoip": "https://m.example/https://github.com/a/b/releases/download/v1/geoip.dat"}}
	if !reflect.DeepEqual(needs, want) {
		t.Errorf("needs = %+v, want %+v", needs, want)
	}
	if update != (GeoUpdate{Auto: true, Interval: 168 * time.Hour}) {
		t.Errorf("update = %+v", update)
	}

	// 没有写时为内核的默认值：不自动更新，间隔 24 小时。
	needs, update, err = GeoSettings([]byte("mode: rule\nrules:\n  - MATCH,DIRECT\n"))
	if err != nil || needs.URLs != nil || update != (GeoUpdate{Interval: 24 * time.Hour}) {
		t.Errorf("defaults: %+v, %+v, %v", needs, update, err)
	}
}
