package compiler

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkCompileStaticLSMLRepresentative(b *testing.B) {
	var layout strings.Builder
	layout.WriteString(`{"kind":"frame","size":{"w":1920,"h":1080},"children":[`)
	for i := 0; i < 250; i++ {
		if i != 0 {
			layout.WriteByte(',')
		}
		fmt.Fprintf(&layout, `{"kind":"text","id":"text-%03d","position":{"x":0,"y":%d},"size":{"w":320,"h":48},"style":{"fontSize":28,"fontWeight":600,"color":"#f2f2f2","shadow":{"x":0,"y":2,"blur":12,"color":"#000000"}}}`, i, i*48)
	}
	layout.WriteString(`]}`)
	raw := []byte(`{"layout":` + layout.String() + `,"defaults":{"title":"Match intro"},"assets":{"allowedHosts":[]}}`)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := CompileStaticLSML(raw, "scene", "version", "https://assets.example.test/scene-assets"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRewriteJSONUntouched(b *testing.B) {
	raw := []byte(`{"fontSize":28,"fontWeight":600,"color":"#f2f2f2","shadow":{"x":0,"y":2,"blur":12,"color":"#000000"},"layout":[1,2,3,4,5,6,7,8]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := rewriteJSON(raw, "https://assets.example.test/scene-assets"); err != nil {
			b.Fatal(err)
		}
	}
}
