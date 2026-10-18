package protocol

import (
	"testing"
)

func bp(b bool) *bool { return &b }

// TestEncodeOverlayApps pins the on-wire shape of the overlay_apps frame:
// { v, type, apps{}, ts? }, no seq. Optional dimensions are omitted when nil.
func TestEncodeOverlayApps(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "single dimension omitted when nil",
			in: OverlayApps{
				Apps: map[string]OverlayAppState{
					"cam": {Running: bp(true)},
				},
			},
			want: `{"v":1,"type":"overlay_apps","apps":{"cam":{"running":true}}}`,
		},
		{
			name: "both dimensions",
			in: OverlayApps{
				Apps: map[string]OverlayAppState{
					"cam": {Running: bp(false), OnAir: bp(true)},
				},
			},
			want: `{"v":1,"type":"overlay_apps","apps":{"cam":{"running":false,"on_air":true}}}`,
		},
		{
			// apps is required and MUST encode as {} — never null.
			name: "empty apps render as object",
			in:   OverlayApps{},
			want: `{"v":1,"type":"overlay_apps","apps":{}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("encode mismatch\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestOverlayAppsRoundTrip proves a server-emitted overlay_apps decodes back to
// the same typed frame (the parity check a runtime relies on), preserving the
// "dimension absent" (nil) vs "set to false" distinction.
func TestOverlayAppsRoundTrip(t *testing.T) {
	orig := OverlayApps{
		Apps: map[string]OverlayAppState{
			"cam": {Running: bp(true), OnAir: bp(false)},
			"lt":  {Running: bp(true)}, // on_air absent
		},
	}
	raw, err := Encode(orig)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeServer(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := decoded.(*OverlayApps)
	if !ok {
		t.Fatalf("decoded type = %T, want *OverlayApps", decoded)
	}
	if got.Type != TypeOverlayApps || got.V != Version {
		t.Fatalf("envelope wrong: v=%d type=%q", got.V, got.Type)
	}
	cam := got.Apps["cam"]
	if cam.Running == nil || !*cam.Running || cam.OnAir == nil || *cam.OnAir {
		t.Fatalf("cam round-trip wrong: %+v", cam)
	}
	lt := got.Apps["lt"]
	if lt.Running == nil || !*lt.Running || lt.OnAir != nil {
		t.Fatalf("lt on_air must stay absent (nil): %+v", lt)
	}
}
