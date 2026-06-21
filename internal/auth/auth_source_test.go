package auth

import (
	"net/http"
	"reflect"
	"testing"
)

// TestHeaderAuthSource_ParityWithFromHeaders proves the default
// AuthSource (antenne profile, ADR 016 §3.2/§3.3) is byte-for-byte the
// existing package-level FromHeaders for every shape of trust header.
// requireOperator and every WS/HTTP gate keep their exact behaviour:
// the AuthSource seam only changes WHO derives the Identity, never the
// derivation itself.
func TestHeaderAuthSource_ParityWithFromHeaders(t *testing.T) {
	var src AuthSource = HeaderAuthSource{}

	cases := map[string]http.Header{
		"empty": {},
		"operator": {
			"X-Authenticated-User": {"u-1"},
			"X-Authenticated-Role": {"operator"},
		},
		"admin-mixed-case-role": {
			"X-Authenticated-User": {"u-2"},
			"X-Authenticated-Role": {"Admin"},
		},
		"service-with-paths": {
			"X-Authenticated-User":  {"svc-quasar"},
			"X-Authenticated-Role":  {"service"},
			"X-Authenticated-Paths": {"quasar.credentials.read, foo.bar.* "},
		},
		"viewer": {
			"X-Authenticated-User": {"u-3"},
			"X-Authenticated-Role": {"viewer"},
		},
	}

	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			want := FromHeaders(h)
			got := src.FromHeaders(h)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("HeaderAuthSource diverges from FromHeaders:\n got=%+v\nwant=%+v", got, want)
			}
		})
	}
}
