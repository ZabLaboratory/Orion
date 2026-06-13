package runtime

import (
	"encoding/json"
	"testing"
)

// TestCompute_SourceRead drives core.source.read@1 through the REAL compute
// registry (the exact fn recompute calls) — the executable proof the
// conformance matrix points at (conformance.go Test: "TestCompute_SourceRead").
// ADR 012 Option B: the compute is a total pure function over the
// compiler-resolved `__resolved_source` config; it returns that object
// verbatim as the node's single value (Option A multi-output projection),
// with NO I/O, NO client, NO Scene.
func TestCompute_SourceRead(t *testing.T) {
	fn, err := NewComputeRegistry().Get("core.source.read@1")
	if err != nil {
		t.Fatalf("core.source.read@1 not registered: %v", err)
	}

	resolved := `{"name":"leaguepedia_feed","kind":"http-poll","descriptor":{"label":"Leaguepedia feed","target_paths":["__inputs.feed.leagues"],"frequency_hz":5,"channel":null}}`

	t.Run("returns the resolved source object verbatim", func(t *testing.T) {
		config := map[string]json.RawMessage{
			"source_id":         json.RawMessage(`"leaguepedia_feed"`),
			"__resolved_source": json.RawMessage(resolved),
		}
		out, err := fn(nil, config)
		if err != nil {
			t.Fatalf("compute errored: %v", err)
		}
		if !json.Valid(out) {
			t.Fatalf("output not valid JSON: %s", out)
		}
		// The whole __resolved_source object is the value (Option A); a
		// downstream get-field projects name/kind/descriptor/config.
		var got, want map[string]any
		_ = json.Unmarshal(out, &got)
		_ = json.Unmarshal([]byte(resolved), &want)
		for _, k := range []string{"name", "kind", "descriptor"} {
			if _, ok := got[k]; !ok {
				t.Errorf("output missing %q pin source: %s", k, out)
			}
		}
		if got["name"] != "leaguepedia_feed" || got["kind"] != "http-poll" {
			t.Errorf("output name/kind wrong: %s", out)
		}
	})

	t.Run("ignores inputs (seed declares none)", func(t *testing.T) {
		config := map[string]json.RawMessage{"__resolved_source": json.RawMessage(resolved)}
		inputs := map[string]json.RawMessage{"junk": json.RawMessage(`1`)}
		out, err := fn(inputs, config)
		if err != nil {
			t.Fatalf("compute errored: %v", err)
		}
		if string(out) != resolved {
			t.Errorf("inputs leaked into output: %s", out)
		}
	})

	t.Run("total: missing __resolved_source yields null, never errors", func(t *testing.T) {
		// The compiler's push-time SOURCE_NOT_DECLARED gate makes this
		// unreachable in practice; the runtime stays total regardless
		// (core.db.* totality rule).
		out, err := fn(nil, map[string]json.RawMessage{"source_id": json.RawMessage(`"x"`)})
		if err != nil {
			t.Fatalf("compute must be total, got error: %v", err)
		}
		if string(out) != `null` {
			t.Errorf("missing resolved source: got %s, want null", out)
		}
		// nil config too.
		out, err = fn(nil, nil)
		if err != nil || string(out) != `null` {
			t.Fatalf("nil config: out=%s err=%v, want null/nil", out, err)
		}
	})
}
