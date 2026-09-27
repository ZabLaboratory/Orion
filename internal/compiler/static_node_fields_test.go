package compiler

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAdaptStaticNodeJSONMatchesDecodedMap(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "duplicate and escaped keys",
			raw: []byte(`{"kind":"ignored","ki\u006ed":"frame","id":"old","id":"scene",` +
				`"text":"old","te\u0078t":"last","bind":{"text":"$.old"},` +
				`"bind":{"text":"$.last"},"children":[{"noKind":true}],` +
				`"children":[false,null,{"kind":"text","text":"child"}],` +
				`"animations":{"ignored":true}}`),
		},
		{
			name: "invalid UTF-8 property key",
			raw:  append(append([]byte(`{"kind":"frame","`), 0xff), []byte(`":"value"}`)...),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(tt.raw, &decoded); err != nil {
				t.Fatalf("decode reference map: %v", err)
			}
			wantTouched := false
			want, err := adaptStaticNode(decoded, "https://assets.example.test", &wantTouched)
			if err != nil {
				t.Fatalf("adapt decoded map: %v", err)
			}
			gotTouched := false
			got, scanned, err := adaptStaticNodeJSON(tt.raw, "https://assets.example.test", &gotTouched)
			if err != nil {
				t.Fatalf("adapt scanned object: %v", err)
			}
			if !scanned {
				t.Fatal("valid object unexpectedly used the compatibility fallback")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("scanned result differs from decoded map\n got: %#v\nwant: %#v", got, want)
			}
			if gotTouched != wantTouched {
				t.Fatalf("assetsTouched differs: got %v, want %v", gotTouched, wantTouched)
			}
		})
	}
}

func FuzzAdaptStaticNodeJSONParity(f *testing.F) {
	f.Add([]byte(`{"kind":"frame","children":[{"kind":"text","text":"ok"}]}`))
	f.Add([]byte(`{"kind":"ignored","ki\u006ed":"frame","id":"a","id":"b"}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
			t.Skip()
		}
		wantTouched := false
		want, wantErr := adaptStaticNode(decoded, "https://assets.example.test", &wantTouched)
		gotTouched := false
		got, scanned, gotErr := adaptStaticNodeJSON(raw, "https://assets.example.test", &gotTouched)
		if !scanned {
			got, gotErr = adaptStaticNode(decoded, "https://assets.example.test", &gotTouched)
		}
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("error mismatch: got %v, want %v", gotErr, wantErr)
		}
		if wantErr != nil && wantErr.Error() != gotErr.Error() {
			t.Fatalf("different errors: got %v, want %v", gotErr, wantErr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("adapted node mismatch\n got: %#v\nwant: %#v", got, want)
		}
		if gotTouched != wantTouched {
			t.Fatalf("assetsTouched differs: got %v, want %v", gotTouched, wantTouched)
		}
	})
}
