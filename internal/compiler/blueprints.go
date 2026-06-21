package compiler

import (
	"errors"
	"sort"
)

// ErrEnvelopeBlueprintConflict is returned by NormalizeBlueprints when an
// envelope sets BOTH the legacy BlueBlueprintID and the new Blueprints list
// (ADR 001 §3.1, the "set + non-empty" row of the normalisation table). The
// API layer maps it to 400 ENVELOPE_BLUEPRINT_CONFLICT BEFORE Compile runs —
// it is an envelope-shape error, not a compile diagnostic, so it must not be
// folded into the 422 COMPILE_FAILED bag.
var ErrEnvelopeBlueprintConflict = errors.New("ENVELOPE_BLUEPRINT_CONFLICT")

// NormalizeBlueprints folds the dual-shape envelope (legacy singular
// BlueBlueprintID vs. new plural Blueprints) into the single canonical list
// the compiler walks (ADR 001 §3.1). It is the ONE place the back-compat
// table lives; both the API layer (for the 400 conflict) and Compile call it,
// so the two can never diverge.
//
//	blue_blueprint_id | blueprints[] | normalised list
//	------------------|--------------|---------------------------------
//	"" / "none" / —   | empty        | []                  (blueprint-free)
//	set               | empty        | [{Key:"", ID:<id>}] (legacy single)
//	—                 | non-empty    | the list, verbatim (sorted by Key)
//	set               | non-empty    | ErrEnvelopeBlueprintConflict
//
// The list is returned sorted by Key so scene_version is stable regardless of
// the authored order of blueprints[] (§3.5). The legacy/empty cases preserve
// the pre-001 byte-exact behaviour: the empty case yields a nil list (the
// per-blueprint loop runs zero times → zero-value graph, issue #28); the
// legacy case yields a single ref keyed "" (empty prefix → leaf paths
// byte-identical to today, R4 non-regression).
func NormalizeBlueprints(e PushEnvelope) ([]BlueprintRef, error) {
	hasSingular := e.BlueBlueprintID != "" && e.BlueBlueprintID != "none"
	hasPlural := len(e.Blueprints) > 0

	switch {
	case hasSingular && hasPlural:
		return nil, ErrEnvelopeBlueprintConflict
	case hasPlural:
		out := make([]BlueprintRef, len(e.Blueprints))
		copy(out, e.Blueprints)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		return out, nil
	case hasSingular:
		// Legacy single → length-1 list, key "" (empty prefix).
		return []BlueprintRef{{Key: "", ID: e.BlueBlueprintID}}, nil
	default:
		// "" / "none" / absent and no plural → blueprint-free scene.
		return nil, nil
	}
}

// validateBlueprintKeys enforces the envelope-level invariants on the
// normalised list before any fetch (ADR 001 §3.1/§5 R1): keys must be unique.
// A duplicate key is a hard error (DUPLICATE_BLUEPRINT_KEY) because the
// leaf-path namespacing would otherwise silently collide. The empty list
// (legacy/blueprint-free) trivially passes.
func validateBlueprintKeys(refs []BlueprintRef) []Diagnostic {
	var diags []Diagnostic
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		// "_" is reserved as the operator addressing token for the DEFAULT
		// (empty) key (api.defaultBlueprintToken). A non-empty authored key "_"
		// would make that alias ambiguous — reject it. The empty key is fine:
		// it IS the default the token aliases.
		if ref.Key == reservedBlueprintKey {
			diags = append(diags, Diagnostic{
				Code:     ErrReservedBlueprintKey,
				Severity: "error",
				Message: "blueprint key " + quoteKey(ref.Key) +
					" is reserved as the operator default-blueprint addressing token",
				Path: ref.Key,
			})
			continue
		}
		if _, dup := seen[ref.Key]; dup {
			diags = append(diags, Diagnostic{
				Code:     ErrDuplicateBlueprintKey,
				Severity: "error",
				Message:  "duplicate blueprint key " + quoteKey(ref.Key),
				Path:     ref.Key,
			})
			continue
		}
		seen[ref.Key] = struct{}{}
	}
	return diags
}

// reservedBlueprintKey mirrors api.defaultBlueprintToken: the HTTP addressing
// token for the default (empty/legacy) blueprint key. The compiler package
// cannot import api (would cycle), so the constant is duplicated; a contract
// test (api/operator_route_keying_test.go) asserts the two stay equal.
const reservedBlueprintKey = "_"

// declaredKeys returns the set of blueprint keys the normalised list declares.
// Used by binding validation (§3.3) to reject a component binding whose
// leading dotted segment names no declared key.
func declaredKeys(refs []BlueprintRef) map[string]struct{} {
	keys := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		keys[ref.Key] = struct{}{}
	}
	return keys
}

// quoteKey renders a key for diagnostics; the empty (legacy) key shows as ""
// so a message about it is not blank.
func quoteKey(k string) string { return "\"" + k + "\"" }
