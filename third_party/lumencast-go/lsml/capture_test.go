package lsml_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Lumencast/lumencast-go/lsml"
)

// decode is a small helper : conformance bundles reach the validator as
// decoded JSON, not as typed structs.
func decode(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return v
}

func TestCheckZabCaptureNodes(t *testing.T) {
	tests := []struct {
		name    string
		layout  string
		wantErr string // substring ; "" means the layout MUST validate
	}{
		{
			name: "visual kind with size validates",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.webcam",
				 "x-zab.deviceRef":"primary-cam","size":{"w":640,"h":360}}]}`,
		},
		{
			name: "audio kind may omit size",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.mic",
				 "x-zab.deviceRef":"main-mic"}]}`,
		},
		{
			name: "system_audio may omit size",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.system_audio",
				 "x-zab.deviceRef":"desktop-out"}]}`,
		},
		{
			name: "media.game with size validates",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.game",
				 "x-zab.deviceRef":"league-client","size":{"w":1920,"h":1080}}]}`,
		},
		{
			// RFC-0001 Amendment 2 §A2.4 — the visual set is a SECOND
			// set : media.file joined the enum AND the visual set.
			name: "media.file without size is rejected",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.file",
				 "x-zab.deviceRef":"intro-sting"}]}`,
			wantErr: "MUST declare `size`",
		},
		{
			name: "media.game without size is rejected",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.game",
				 "x-zab.deviceRef":"league-client"}]}`,
			wantErr: "MUST declare `size`",
		},
		{
			name: "physical device id is rejected",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.webcam",
				 "x-zab.deviceRef":"video:0","size":{"w":640,"h":360}}]}`,
			wantErr: "logical alias",
		},
		{
			name: "uppercase deviceRef is rejected",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.webcam",
				 "x-zab.deviceRef":"Primary-Cam","size":{"w":640,"h":360}}]}`,
			wantErr: "logical alias",
		},
		{
			name: "deviceRef starting with a digit is rejected",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.mic",
				 "x-zab.deviceRef":"0-mic"}]}`,
			wantErr: "logical alias",
		},
		{
			name: "missing sourceKind is rejected",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.deviceRef":"primary-cam",
				 "size":{"w":640,"h":360}}]}`,
			wantErr: "x-zab.sourceKind",
		},
		{
			name: "unknown sourceKind is rejected",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.hologram",
				 "x-zab.deviceRef":"primary-cam","size":{"w":640,"h":360}}]}`,
			wantErr: "unknown `x-zab.sourceKind`",
		},
		{
			name: "missing deviceRef is rejected",
			layout: `{"kind":"frame","children":[
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.webcam",
				 "size":{"w":640,"h":360}}]}`,
			wantErr: "x-zab.deviceRef",
		},
		{
			// The walk speaks for x-zab.capture alone : an unknown core
			// primitive is left to lsml.Validate, not rejected here.
			name:   "non-capture nodes are left untouched",
			layout: `{"kind":"frame","children":[{"kind":"future-primitive","whatever":1}]}`,
		},
		{
			name: "nested inside a repeat template is reached",
			layout: `{"kind":"repeat","template":
				{"kind":"x-zab.capture","x-zab.sourceKind":"media.file",
				 "x-zab.deviceRef":"intro-sting"}}`,
			wantErr: "MUST declare `size`",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := lsml.CheckZabCaptureNodes(decode(t, tc.layout))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want valid, got error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// A deviceRef of exactly 64 chars is the boundary of
// ^[a-z][a-z0-9-]{0,63}$ ; 65 is over it.
func TestDeviceRefLengthBoundary(t *testing.T) {
	node := func(ref string) any {
		return decode(t, `{"kind":"x-zab.capture","x-zab.sourceKind":"media.mic","x-zab.deviceRef":"`+ref+`"}`)
	}
	if err := lsml.CheckZabCaptureNodes(node("a" + strings.Repeat("b", 63))); err != nil {
		t.Fatalf("64-char deviceRef must validate, got %v", err)
	}
	if err := lsml.CheckZabCaptureNodes(node("a" + strings.Repeat("b", 64))); err == nil {
		t.Fatal("65-char deviceRef must be rejected")
	}
	if err := lsml.CheckZabCaptureNodes(node("")); err == nil {
		t.Fatal("empty deviceRef must be rejected")
	}
}
