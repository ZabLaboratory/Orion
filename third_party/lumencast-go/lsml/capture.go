package lsml

import (
	"fmt"
	"strings"
)

// ZabCaptureKind is the one vendor-prefixed `kind` this package
// recognises : the Zab capture placeholder of RFC-0001.
const ZabCaptureKind = "x-zab.capture"

// captureVisualKinds are the `x-zab.sourceKind` values whose box is
// painted. `size` is mandatory for them : the geometry is what lets the
// consuming app position the native source over the box the author drew
// (RFC-0001 Amendment 2 §A2.2 / §A2.4).
var captureVisualKinds = []string{
	"media.webcam",
	"media.screen",
	"media.window",
	"media.file",
	"media.game",
}

// captureAudioKinds are the audio-only `x-zab.sourceKind` values — a
// zero-area inert box is valid, so `size` MAY be omitted (§A2.2).
var captureAudioKinds = []string{
	"media.app_audio",
	"media.system_audio",
	"media.mic",
}

// CheckZabCaptureNodes validates every `x-zab.capture` node reachable
// from root against RFC-0001 (+ Amendment 2), leaving every other node
// untouched. root is a decoded JSON value (map[string]any / []any).
//
// Deliberately narrower than Validate : a caller holding an untrusted
// inline layout can enforce the vendor contract without also taking a
// position on core primitives it may not know yet — the bundles that
// reach it come from several LSML minors.
func CheckZabCaptureNodes(root any) error {
	stack := []any{root}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		obj, ok := node.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := obj["kind"].(string); kind == ZabCaptureKind {
			if err := checkZabCapture(obj); err != nil {
				return err
			}
		}
		if children, ok := obj["children"].([]any); ok {
			stack = append(stack, children...)
		}
		if tmpl, ok := obj["template"]; ok {
			stack = append(stack, tmpl)
		}
	}
	return nil
}

// checkZabCapture applies the RFC-0001 prop rules to a single
// `x-zab.capture` node.
func checkZabCapture(obj map[string]any) error {
	sourceKind, ok := obj["x-zab.sourceKind"].(string)
	if !ok || sourceKind == "" {
		return fmt.Errorf("`%s` MUST declare `x-zab.sourceKind` (RFC-0001)", ZabCaptureKind)
	}
	visual := contains(captureVisualKinds, sourceKind)
	if !visual && !contains(captureAudioKinds, sourceKind) {
		return fmt.Errorf("unknown `x-zab.sourceKind` %q (RFC-0001 A2 §A2.2: %s, %s)",
			sourceKind,
			strings.Join(captureVisualKinds, ", "),
			strings.Join(captureAudioKinds, ", "))
	}

	deviceRef, ok := obj["x-zab.deviceRef"].(string)
	if !ok {
		return fmt.Errorf("`%s` MUST declare `x-zab.deviceRef` (RFC-0001)", ZabCaptureKind)
	}
	if !isLogicalDeviceRef(deviceRef) {
		return fmt.Errorf("`x-zab.deviceRef` %q MUST be a logical alias matching "+
			"^[a-z][a-z0-9-]{0,63}$ — never a physical device id (RFC-0001)", deviceRef)
	}

	// §A2.4 — the visual set is a SECOND set, not a subset check on the
	// enum : extending the enum alone would let a `media.file` with no
	// geometry through as a zero-area media box.
	if _, hasSize := obj["size"]; visual && !hasSize {
		return fmt.Errorf("`%s` of visual kind %q MUST declare `size` "+
			"(RFC-0001 Amendment 2 §A2.2)", ZabCaptureKind, sourceKind)
	}
	return nil
}

// isLogicalDeviceRef implements `^[a-z][a-z0-9-]{0,63}$` — the
// hash-safe logical alias grammar.
func isLogicalDeviceRef(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
