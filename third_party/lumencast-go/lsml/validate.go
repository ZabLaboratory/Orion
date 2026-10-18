package lsml

import (
	"encoding/json"
	"fmt"

	"github.com/Lumencast/lumencast-go/protocol"
)

// Issue is one validation finding.
type Issue struct {
	// Path is the JSON path inside the bundle (e.g. "layout.children[0].bind.value").
	Path string
	// Message is human-readable.
	Message string
}

// Report aggregates errors and warnings from a single Validate call.
type Report struct {
	Errors   []Issue
	Warnings []Issue
}

// Validate runs the LSML 1.0 schema, animation discipline,
// accessibility, and bind-path checks. Returns a non-empty Report on
// any finding ; len(Report.Errors) == 0 means the bundle passes.
func Validate(b *Bundle) *Report {
	r := &Report{}

	if b == nil {
		r.Errors = append(r.Errors, Issue{"", "nil bundle"})
		return r
	}
	if b.LSML != Version {
		r.Errors = append(r.Errors, Issue{"lsml", fmt.Sprintf("unsupported version %q (this validator implements %s)", b.LSML, Version)})
	}
	if b.SceneID == "" {
		r.Errors = append(r.Errors, Issue{"scene_id", "required"})
	}
	if b.SceneVersion == "" {
		r.Errors = append(r.Errors, Issue{"scene_version", "required"})
	}
	if len(b.Layout) == 0 {
		r.Errors = append(r.Errors, Issue{"layout", "required"})
	} else {
		validateNode(r, "layout", b.Layout, b.Assets)
	}

	for i, oi := range b.OperatorInputs {
		path := fmt.Sprintf("operator_inputs[%d]", i)
		if err := protocol.LeafPath(oi.Path).Validate(); err != nil {
			r.Errors = append(r.Errors, Issue{path + ".path", err.Error()})
		}
		if oi.Label == "" {
			r.Errors = append(r.Errors, Issue{path + ".label", "required"})
		}
		if oi.Type == "" {
			r.Errors = append(r.Errors, Issue{path + ".type", "required"})
		}
		if len(oi.WritableBy) == 0 {
			r.Errors = append(r.Errors, Issue{path + ".writable_by", "required, non-empty"})
		}
	}

	for path := range b.Defaults {
		if err := protocol.LeafPath(path).Validate(); err != nil {
			r.Errors = append(r.Errors, Issue{"defaults." + path, err.Error()})
		}
	}

	return r
}

// validateNode recursively inspects a primitive subtree.
func validateNode(r *Report, base string, raw json.RawMessage, assets *Assets) {
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		r.Errors = append(r.Errors, Issue{base, "invalid JSON: " + err.Error()})
		return
	}
	kind, _ := node["kind"].(string)
	if kind == "" {
		r.Errors = append(r.Errors, Issue{base + ".kind", "required"})
		return
	}
	if !knownPrimitive(kind) {
		r.Errors = append(r.Errors, Issue{base + ".kind", fmt.Sprintf("unknown primitive %q (LSML 1.0 supports : stack, grid, frame, text, image, shape, media, repeat)", kind)})
		return
	}

	// Bind paths must be valid LeafPath. Scope placeholders ({name})
	// are accepted because primitives may be inside a repeat template.
	if bind, ok := node["bind"].(map[string]any); ok {
		for k, v := range bind {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if err := protocol.LeafPath(s).ValidateTemplate(); err != nil {
				r.Errors = append(r.Errors, Issue{base + ".bind." + k, err.Error()})
			}
		}
	}

	// Animate discipline : transform / opacity / filter only.
	if anim, ok := node["animate"].(map[string]any); ok {
		for k := range anim {
			if k == "transition" || k == "transform" || k == "opacity" || k == "filter" || k == "respectsReducedMotion" {
				continue
			}
			r.Errors = append(r.Errors, Issue{base + ".animate." + k,
				"only transform, opacity, filter are animatable in LSML 1.0"})
		}
	}

	// Accessibility : image MUST have alt.
	if kind == "image" {
		if _, hasAlt := node["alt"]; !hasAlt {
			r.Errors = append(r.Errors, Issue{base + ".alt",
				"required for image (LSML 1.0 § 13). Use \"\" for decorative images."})
		}
		// Asset host check.
		if assets != nil && len(assets.AllowedHosts) > 0 {
			// Bind src → can't statically check the URL ; warn only.
			r.Warnings = append(r.Warnings, Issue{base + ".bind.src",
				"image URLs are bound at runtime ; ensure your operator UI restricts to allowedHosts"})
		}
	}

	// Recurse into children.
	if children, ok := node["children"].([]any); ok {
		for i, child := range children {
			b, _ := json.Marshal(child)
			validateNode(r, fmt.Sprintf("%s.children[%d]", base, i), b, assets)
		}
	}
	if tmpl, ok := node["template"]; ok && kind == "repeat" {
		b, _ := json.Marshal(tmpl)
		validateNode(r, base+".template", b, assets)
	}
}

func knownPrimitive(kind string) bool {
	switch kind {
	case "stack", "grid", "frame", "text", "image", "shape", "media", "repeat":
		return true
	}
	return false
}
