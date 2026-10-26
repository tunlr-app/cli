package config

import "testing"

func TestDetectGeo(t *testing.T) {
	tests := []struct {
		name, cfg string
		want      GeoNeeds
	}{
		{"none", "rules:\n  - DOMAIN-SUFFIX,cn,DIRECT\n  - MATCH,DIRECT\n", GeoNeeds{}},
		{"geox-url ignored", "geox-url:\n  geoip: https://x/geoip.dat\n  geosite: https://x/geosite.dat\n  asn: https://x/GeoLite2-ASN.mmdb\ngeo-auto-update: true\n", GeoNeeds{}},
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
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
