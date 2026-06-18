package compiler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Lumencast/lumencast-go/lsml"
)

// bundleFrom builds an lsml.Bundle from a raw layout JSON literal and an
// optional allowlist. Keeps the pathological fixtures readable as the JSON
// an author would actually push, while exercising the gate's tree walk.
func bundleFrom(t *testing.T, layout string, allowedHosts ...string) *lsml.Bundle {
	t.Helper()
	b := &lsml.Bundle{
		LSML:         "1.2",
		SceneID:      "scene-test",
		SceneVersion: "sha256:0",
		Layout:       json.RawMessage(layout),
	}
	if len(allowedHosts) > 0 {
		b.Assets = &lsml.Assets{AllowedHosts: allowedHosts}
	}
	return b
}

func hasCode(d Diagnostics, code DiagnosticCode) bool {
	for _, it := range d.Items {
		if it.Code == code {
			return true
		}
	}
	return false
}

func errorsOnly(d Diagnostics) bool {
	return d.HasErrors()
}

// --- Nominal: a legitimate 1.2 bundle passes the gate. ---

func TestGate_Nominal_Passes(t *testing.T) {
	layout := `{
		"kind": "frame", "id": "root",
		"backgrounds": [
			{"kind": "image", "src": "https://cdn.example.com/cover.png", "objectFit": "cover", "blendMode": "multiply"}
		],
		"children": [
			{"kind": "shape", "id": "ruby", "blendMode": "screen",
			 "fills": [{"kind": "image", "src": "data:image/png;base64,AAAA", "objectFit": "contain"}]},
			{"kind": "image", "id": "logo", "src": "https://cdn.example.com/logo.png",
			 "mask": {"source": {"kind": "shape", "ref": "ruby"}, "type": "alpha", "op": "intersect"}}
		]
	}`
	d := GateLSMLBundle(bundleFrom(t, layout, "cdn.example.com"))
	if d.HasErrors() {
		t.Fatalf("nominal bundle refused: %+v", d.Items)
	}
}

func TestGate_NoAssets_NoRemoteSrc_Passes(t *testing.T) {
	layout := `{"kind":"frame","children":[{"kind":"shape","fills":[{"kind":"solid","color":"#fff"}]}]}`
	d := GateLSMLBundle(bundleFrom(t, layout))
	if d.HasErrors() {
		t.Fatalf("data-free bundle refused: %+v", d.Items)
	}
}

// --- T1/T2: hostile / out-of-allowlist src refused. ---

func TestGate_SrcHostNotAllowed(t *testing.T) {
	layout := `{"kind":"image","src":"https://evil.com/x.png"}`
	d := GateLSMLBundle(bundleFrom(t, layout, "cdn.example.com"))
	if !hasCode(d, GateHostNotAllowed) {
		t.Fatalf("expected GATE_HOST_NOT_ALLOWED, got %+v", d.Items)
	}
}

func TestGate_SrcSubstringHostNotAllowed(t *testing.T) {
	// Substring/look-alike must NOT match — host equality on the parsed host.
	for _, host := range []string{
		"https://cdn.example.com.evil.com/x.png",
		"https://cdn-example.com/x.png",
		"https://cdn.example.com@evil.com/x.png",
	} {
		layout := `{"kind":"image","src":"` + host + `"}`
		d := GateLSMLBundle(bundleFrom(t, layout, "cdn.example.com"))
		if !errorsOnly(d) {
			t.Fatalf("look-alike host %q was NOT refused: %+v", host, d.Items)
		}
	}
}

func TestGate_HostileScheme(t *testing.T) {
	for _, src := range []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=",
		"file:///etc/passwd",
		"blob:https://x/y",
		"//evil.com/x.png",
	} {
		layout := `{"kind":"image","src":"` + src + `"}`
		d := GateLSMLBundle(bundleFrom(t, layout, "cdn.example.com"))
		if !errorsOnly(d) {
			t.Fatalf("hostile scheme %q was NOT refused: %+v", src, d.Items)
		}
	}
}

func TestGate_EmptyAllowlistDeniesRemote(t *testing.T) {
	layout := `{"kind":"image","src":"https://cdn.example.com/x.png"}`
	d := GateLSMLBundle(bundleFrom(t, layout)) // no assets block
	if !hasCode(d, GateHostNotAllowed) {
		t.Fatalf("empty allowlist must deny remote host: %+v", d.Items)
	}
}

func TestGate_RejectedURLNeverEchoed(t *testing.T) {
	// Bastion R9: the rejected URL must not appear in any diagnostic message.
	const secret = "https://evil.com/super-secret-token-abc123.png"
	layout := `{"kind":"image","src":"` + secret + `"}`
	d := GateLSMLBundle(bundleFrom(t, layout, "cdn.example.com"))
	for _, it := range d.Items {
		if strings.Contains(it.Message, "evil.com") || strings.Contains(it.Message, "abc123") {
			t.Fatalf("diagnostic echoed the rejected URL: %q", it.Message)
		}
	}
}

// --- T4: hostile / out-of-enum values refused. ---

func TestGate_BlendModeHostile(t *testing.T) {
	layout := `{"kind":"shape","blendMode":"url(javascript:alert(1))"}`
	d := GateLSMLBundle(bundleFrom(t, layout))
	if !hasCode(d, GateEnumNotAllowed) {
		t.Fatalf("hostile blendMode not refused: %+v", d.Items)
	}
}

func TestGate_FillBlendOutOfEnum(t *testing.T) {
	layout := `{"kind":"shape","fills":[{"kind":"solid","color":"#fff","blendMode":"PASS_THROUGH"}]}`
	d := GateLSMLBundle(bundleFrom(t, layout))
	if !hasCode(d, GateEnumNotAllowed) {
		t.Fatalf("fill blendMode out of enum not refused: %+v", d.Items)
	}
}

func TestGate_ObjectFitOutOfEnum(t *testing.T) {
	layout := `{"kind":"image","src":"data:image/png;base64,AA","objectFit":"sneaky"}`
	d := GateLSMLBundle(bundleFrom(t, layout))
	if !hasCode(d, GateEnumNotAllowed) {
		t.Fatalf("objectFit out of enum not refused: %+v", d.Items)
	}
}

func TestGate_MaskEnumsOutOfList(t *testing.T) {
	layout := `{"kind":"frame","children":[
		{"kind":"shape","id":"s"},
		{"kind":"image","mask":{"source":{"kind":"shape","ref":"s"},"type":"sneaky","op":"xor"}}
	]}`
	d := GateLSMLBundle(bundleFrom(t, layout))
	if !hasCode(d, GateEnumNotAllowed) {
		t.Fatalf("mask.type/op out of enum not refused: %+v", d.Items)
	}
}

// --- #K: dangling ref + anti-cycle (mask→shape→mask). ---

func TestGate_DanglingMaskRef(t *testing.T) {
	layout := `{"kind":"image","mask":{"source":{"kind":"shape","ref":"does-not-exist"},"type":"alpha","op":"intersect"}}`
	d := GateLSMLBundle(bundleFrom(t, layout))
	if !hasCode(d, GateDanglingMaskRef) {
		t.Fatalf("dangling mask ref not refused: %+v", d.Items)
	}
}

func TestGate_MaskCycle(t *testing.T) {
	// a is masked by b; b is masked by a → mask→shape→mask cycle.
	layout := `{"kind":"frame","children":[
		{"kind":"shape","id":"a","mask":{"source":{"kind":"shape","ref":"b"},"type":"alpha","op":"intersect"}},
		{"kind":"shape","id":"b","mask":{"source":{"kind":"shape","ref":"a"},"type":"alpha","op":"intersect"}}
	]}`
	d := GateLSMLBundle(bundleFrom(t, layout))
	if !hasCode(d, GateMaskCycle) {
		t.Fatalf("mask cycle not refused: %+v", d.Items)
	}
}

func TestGate_MaskSelfCycle(t *testing.T) {
	layout := `{"kind":"shape","id":"a","mask":{"source":{"kind":"shape","ref":"a"},"type":"alpha","op":"intersect"}}`
	d := GateLSMLBundle(bundleFrom(t, layout))
	if !hasCode(d, GateMaskCycle) {
		t.Fatalf("mask self-cycle not refused: %+v", d.Items)
	}
}

func TestGate_MaskImageSourceGated(t *testing.T) {
	layout := `{"kind":"image","mask":{"source":{"kind":"image","src":"https://evil.com/m.png"},"type":"alpha","op":"intersect"}}`
	d := GateLSMLBundle(bundleFrom(t, layout, "cdn.example.com"))
	if !hasCode(d, GateHostNotAllowed) {
		t.Fatalf("mask image source host not gated: %+v", d.Items)
	}
}

// --- T5: complexity budget. Each fixture trips exactly one cap. ---

func TestGate_BudgetBlendNodes(t *testing.T) {
	budget := AuthoringBudget{MaxBlendNodes: 4, MaxMaskDepth: 16, MaxImageNodes: 4096, MaxTotalNodes: 16384}
	var sb strings.Builder
	sb.WriteString(`{"kind":"frame","children":[`)
	for i := 0; i < 10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"kind":"shape","blendMode":"multiply"}`)
	}
	sb.WriteString(`]}`)
	d := gateLSMLBundle(bundleFrom(t, sb.String()), budget)
	if !hasCode(d, GateBudgetExceeded) {
		t.Fatalf("blend-node budget not enforced: %+v", d.Items)
	}
}

func TestGate_BudgetImageNodes(t *testing.T) {
	budget := AuthoringBudget{MaxBlendNodes: 512, MaxMaskDepth: 16, MaxImageNodes: 3, MaxTotalNodes: 16384}
	var sb strings.Builder
	sb.WriteString(`{"kind":"frame","children":[`)
	for i := 0; i < 10; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"kind":"image","src":"data:image/png;base64,AA"}`)
	}
	sb.WriteString(`]}`)
	d := gateLSMLBundle(bundleFrom(t, sb.String()), budget)
	if !hasCode(d, GateBudgetExceeded) {
		t.Fatalf("image-node budget not enforced: %+v", d.Items)
	}
}

func TestGate_BudgetTotalNodes(t *testing.T) {
	budget := AuthoringBudget{MaxBlendNodes: 512, MaxMaskDepth: 16, MaxImageNodes: 4096, MaxTotalNodes: 5}
	var sb strings.Builder
	sb.WriteString(`{"kind":"frame","children":[`)
	for i := 0; i < 20; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"kind":"shape"}`)
	}
	sb.WriteString(`]}`)
	d := gateLSMLBundle(bundleFrom(t, sb.String()), budget)
	if !hasCode(d, GateBudgetExceeded) {
		t.Fatalf("total-node budget not enforced: %+v", d.Items)
	}
}

func TestGate_BudgetMaskDepth(t *testing.T) {
	budget := AuthoringBudget{MaxBlendNodes: 512, MaxMaskDepth: 3, MaxImageNodes: 4096, MaxTotalNodes: 16384}
	// Chain of 6 shapes each masked by the next: depth 6 > cap 3.
	var sb strings.Builder
	sb.WriteString(`{"kind":"frame","children":[`)
	n := 6
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		if i < n-1 {
			sb.WriteString(`{"kind":"shape","id":"m` + itoa(i) + `","mask":{"source":{"kind":"shape","ref":"m` + itoa(i+1) + `"},"type":"alpha","op":"intersect"}}`)
		} else {
			sb.WriteString(`{"kind":"shape","id":"m` + itoa(i) + `"}`)
		}
	}
	sb.WriteString(`]}`)
	d := gateLSMLBundle(bundleFrom(t, sb.String()), budget)
	if !hasCode(d, GateBudgetExceeded) {
		t.Fatalf("mask-depth budget not enforced: %+v", d.Items)
	}
}

// A deep but ACYCLIC mask chain under the cap must NOT freeze and must
// pass — proves the walk terminates and the depth cap is the only refusal.
func TestGate_DeepAcyclicMaskUnderCapPasses(t *testing.T) {
	budget := AuthoringBudget{MaxBlendNodes: 512, MaxMaskDepth: 16, MaxImageNodes: 4096, MaxTotalNodes: 16384}
	var sb strings.Builder
	sb.WriteString(`{"kind":"frame","children":[`)
	n := 10
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		if i < n-1 {
			sb.WriteString(`{"kind":"shape","id":"d` + itoa(i) + `","mask":{"source":{"kind":"shape","ref":"d` + itoa(i+1) + `"},"type":"alpha","op":"intersect"}}`)
		} else {
			sb.WriteString(`{"kind":"shape","id":"d` + itoa(i) + `"}`)
		}
	}
	sb.WriteString(`]}`)
	d := gateLSMLBundle(bundleFrom(t, sb.String()), budget)
	if d.HasErrors() {
		t.Fatalf("deep acyclic mask under cap refused: %+v", d.Items)
	}
}

func TestGate_MalformedLayout(t *testing.T) {
	d := GateLSMLBundle(bundleFrom(t, `{"kind":"frame","children":"not-an-array"}`))
	if !hasCode(d, GateMalformedNode) {
		t.Fatalf("malformed layout not refused: %+v", d.Items)
	}
}

func TestGate_NilBundle(t *testing.T) {
	d := GateLSMLBundle(nil)
	if !d.HasErrors() {
		t.Fatalf("nil bundle must error")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [4]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
