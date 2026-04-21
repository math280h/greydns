package cloudflare

import "testing"

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"api.example.com":  "api.example.com",
		"api.example.com.": "api.example.com",
		"":                 "",
		".":                "",
	}
	for in, want := range cases {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseOwner(t *testing.T) {
	cases := []struct {
		name    string
		comment string
		want    string
		ok      bool
	}{
		{"kind-prefixed service", "[greydns]owner=svc:default/api", "svc:default/api", true},
		{"kind-prefixed ingress", "[greydns]owner=ing:default/api", "ing:default/api", true},
		{"legacy bare ns/name treated as service", "[greydns]owner=default/api", "svc:default/api", true},
		{"missing marker", "owner=default/api", "", false},
		{"unknown kind", "[greydns]owner=foo:default/api", "", false},
		{"empty namespace", "[greydns]owner=/api", "", false},
		{"empty name", "[greydns]owner=default/", "", false},
		{"extra slash", "[greydns]owner=default/api/extra", "", false},
		{"empty suffix", "[greydns]owner=", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseOwner(tc.comment)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("owner = %q, want %q", got, tc.want)
			}
		})
	}
}
