package compiler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// CompileStaticLSML converts the authoring LSML bundle emitted by ZabCanvas
// into the RenderBundle consumed by Solar's broadcast renderer. Static
// scene-intent refs do not have a Blue program, so this conversion is the
// only compilation step on that path.
//
// The returned defaults are the initial state for the LSDP scene. Keeping
// them separate from the render bundle is intentional: Solar resolves
// bindings from the scene snapshot, while the bundle describes the tree.
func CompileStaticLSML(raw []byte, _ string, sceneVersion, assetBaseURL string) ([]byte, map[string]json.RawMessage, error) {
	bundle, passthrough, defaults, err := compileStaticLSML(raw, sceneVersion, assetBaseURL)
	if err != nil {
		return nil, nil, err
	}
	if passthrough != nil {
		return passthrough, defaults, nil
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return nil, nil, fmt.Errorf("encode static RenderBundle: %w", err)
	}
	return encoded, defaults, nil
}

// ErrInvalidStaticRenderBundle distinguishes an invalid already-compiled root
// from an authoring compilation failure, preserving the Preview HTTP contract.
var ErrInvalidStaticRenderBundle = errors.New("invalid static RenderBundle")

// CompileStaticRenderBundle is the in-process counterpart of CompileStaticLSML.
// It avoids encoding then decoding the complete tree for editable Preview. The
// byte API remains authoritative for persisted artifacts and their digests.
func CompileStaticRenderBundle(raw []byte, _ string, sceneVersion, assetBaseURL string) (*RenderBundle, map[string]json.RawMessage, error) {
	bundle, passthrough, defaults, err := compileStaticLSML(raw, sceneVersion, assetBaseURL)
	if err != nil {
		return nil, nil, err
	}
	if passthrough != nil {
		bundle = new(RenderBundle)
		if err := json.Unmarshal(passthrough, bundle); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrInvalidStaticRenderBundle, err)
		}
	} else if defaults != nil {
		// The former JSON roundtrip gave the immutable bundle and mutable scene
		// seeds independent ownership, including each RawMessage byte slice.
		bundle.Defaults = make(map[string]json.RawMessage, len(defaults))
		for path, value := range defaults {
			bundle.Defaults[path] = bytes.Clone(value)
		}
	}
	var source struct {
		LSML string `json:"lsml"`
	}
	if json.Unmarshal(raw, &source) == nil && source.LSML != "" {
		bundle.SourceLSML = bytes.Clone(raw)
	}
	return bundle, defaults, nil
}

func compileStaticLSML(raw []byte, sceneVersion, assetBaseURL string) (*RenderBundle, []byte, map[string]json.RawMessage, error) {
	var source struct {
		Layout           json.RawMessage            `json:"layout"`
		Root             json.RawMessage            `json:"root"`
		Defaults         map[string]json.RawMessage `json:"defaults"`
		OperatorInputs   []OperatorInput            `json:"operator_inputs"`
		ExternalAdapters []ExternalAdapter          `json:"external_adapters"`
		Assets           json.RawMessage            `json:"assets"`
		Profiles         []string                   `json:"profiles"`
		Animations       json.RawMessage            `json:"animations"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, nil, nil, fmt.Errorf("decode static LSML bundle: %w", err)
	}
	// Accept an already compiled bundle for forward compatibility. The
	// current ZabCanvas producer sends layout; this branch avoids a needless
	// second lowering if a future producer sends root directly.
	if len(source.Layout) == 0 && len(source.Root) != 0 {
		defaults, _, err := rewriteDefaults(source.Defaults, assetBaseURL)
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, raw, defaults, nil
	}
	if len(source.Layout) == 0 {
		return nil, nil, nil, errors.New("static LSML bundle has neither layout nor compiled root")
	}

	rawRoot, err := decodeStaticObjectFields(source.Layout)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode static LSML layout: %w", err)
	}

	assetsTouched := false
	root, err := adaptStaticNode(rawRoot, assetBaseURL, &assetsTouched)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("adapt static LSML layout: %w", err)
	}
	animations := source.Animations
	if len(animations) == 0 {
		animations = rawRoot["animations"]
	}
	loweredRoot := lowerRenderTree(root, parseAnimationCatalogue(animations))

	defaults, defaultsTouched, err := rewriteDefaults(source.Defaults, assetBaseURL)
	if err != nil {
		return nil, nil, nil, err
	}
	assetsTouched = assetsTouched || defaultsTouched
	assets, err := rewriteAssets(source.Assets, assetBaseURL, &assetsTouched)
	if err != nil {
		return nil, nil, nil, err
	}

	bundle := RenderBundle{
		SceneVersion:     sceneVersion,
		Root:             loweredRoot,
		OperatorInputs:   source.OperatorInputs,
		ExternalAdapters: source.ExternalAdapters,
		Profiles:         source.Profiles,
		LSMLAssets:       assets,
		Defaults:         defaults,
	}
	return &bundle, nil, defaults, nil
}

var staticAssetRef = regexp.MustCompile(`^assets/([0-9a-fA-F]{64})(?:\.[A-Za-z0-9]+)?$`)

func adaptStaticNodeFields(fields staticNodeFields, assetBaseURL string, assetsTouched *bool) (LayoutNode, error) {
	var node LayoutNode
	if err := json.Unmarshal(fields.kind, &node.Kind); err != nil || node.Kind == "" {
		if err == nil {
			err = errors.New("node kind is empty")
		}
		return LayoutNode{}, err
	}
	if id := fields.id; len(id) != 0 {
		_ = json.Unmarshal(id, &node.ID)
	}

	if fields.props != nil {
		node.Props = fields.props
		for key, value := range node.Props {
			rewritten, touched, err := rewriteJSONValidatedParent(value, assetBaseURL)
			if err != nil {
				return LayoutNode{}, fmt.Errorf("property %q: %w", key, err)
			}
			if touched {
				*assetsTouched = true
			}
			node.Props[key] = rewritten
		}
	}

	for _, bindingRaw := range []json.RawMessage{fields.bind, fields.bindStyle, fields.bindUniversal} {
		if len(bindingRaw) == 0 {
			continue
		}
		var bindings map[string]string
		if err := json.Unmarshal(bindingRaw, &bindings); err != nil {
			continue
		}
		if node.Bindings == nil {
			node.Bindings = make(map[string]string)
		}
		for property, path := range bindings {
			node.Bindings[property] = path
		}
	}
	if len(node.Bindings) == 0 {
		node.Bindings = nil
	}
	if bindings := fields.bindAnimate; len(bindings) != 0 {
		_ = json.Unmarshal(bindings, &node.AnimateBindings)
	}
	if len(node.AnimateBindings) == 0 {
		node.AnimateBindings = nil
	}
	if transitions := fields.animate; len(transitions) != 0 {
		if err := json.Unmarshal(transitions, &node.Transitions); err != nil {
			node.Transitions = nil
		}
	}

	if childrenRaw := fields.children; len(childrenRaw) != 0 {
		originalChildren := len(node.Children)
		originalAssetsTouched := *assetsTouched
		fast, fastErr := scanJSONArrayElements(childrenRaw, func(childRaw []byte) error {
			childNode, scanned, err := adaptStaticNodeJSON(childRaw, assetBaseURL, assetsTouched)
			if err != nil {
				return err
			}
			if !scanned {
				// Preserve the encoding/json behavior for non-object entries and
				// retain its compatibility fallback if a valid object uses a
				// form this scanner does not recognize.
				var decoded map[string]json.RawMessage
				if err := json.Unmarshal(childRaw, &decoded); err != nil || decoded == nil {
					return nil
				}
				childNode, err = adaptStaticNode(decoded, assetBaseURL, assetsTouched)
				if err != nil {
					return err
				}
			}
			node.Children = append(node.Children, childNode)
			return nil
		})
		if fastErr != nil {
			return LayoutNode{}, fastErr
		}
		if !fast {
			// Keep the old tolerant behavior for non-array or malformed values:
			// valid object siblings still compile while non-object entries are
			// ignored.
			node.Children = node.Children[:originalChildren]
			*assetsTouched = originalAssetsTouched
			var children []map[string]json.RawMessage
			if err := json.Unmarshal(childrenRaw, &children); err != nil {
				var childRaws []json.RawMessage
				if json.Unmarshal(childrenRaw, &childRaws) == nil {
					children = make([]map[string]json.RawMessage, 0, len(childRaws))
					for _, childRaw := range childRaws {
						var child map[string]json.RawMessage
						if err := json.Unmarshal(childRaw, &child); err == nil && child != nil {
							children = append(children, child)
						}
					}
				}
			}
			for _, child := range children {
				if child == nil {
					continue
				}
				childNode, err := adaptStaticNode(child, assetBaseURL, assetsTouched)
				if err != nil {
					return LayoutNode{}, err
				}
				node.Children = append(node.Children, childNode)
			}
		}
	}
	return node, nil
}

// decodeStaticObjectFields scans already-validated JSON into RawMessage views
// instead of asking encoding/json to allocate a byte copy for every property.
// source.Layout was validated by the enclosing bundle unmarshal, and the
// returned views keep that owned RawMessage backing array alive.
func decodeStaticObjectFields(raw []byte) (map[string]json.RawMessage, error) {
	if fields, ok := scanJSONObjectFields(raw); ok {
		return fields, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("layout is not an object")
	}
	return fields, nil
}

// scanJSONObjectFields returns values as views into raw. Escaped keys are
// decoded by encoding/json; ordinary keys and all values stay on the
// allocation-light scanner path. Duplicate keys retain encoding/json's
// last-value-wins map semantics.
func scanJSONObjectFields(raw []byte) (map[string]json.RawMessage, bool) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || data[0] != '{' {
		return nil, false
	}
	fields := make(map[string]json.RawMessage, 8)
	i := skipJSONWhitespace(data, 1)
	if i < len(data) && data[i] == '}' {
		return fields, i == len(data)-1
	}
	for i < len(data) {
		keyStart := i
		keyEnd, ok := scanJSONStringEnd(data, keyStart)
		if !ok {
			return nil, false
		}
		keyRaw := data[keyStart:keyEnd]
		var key string
		if bytes.IndexByte(keyRaw, '\\') >= 0 || !utf8.Valid(keyRaw[1:len(keyRaw)-1]) {
			if err := json.Unmarshal(keyRaw, &key); err != nil {
				return nil, false
			}
		} else {
			key = string(keyRaw[1 : len(keyRaw)-1])
		}
		i = skipJSONWhitespace(data, keyEnd)
		if i >= len(data) || data[i] != ':' {
			return nil, false
		}
		valueStart := skipJSONWhitespace(data, i+1)
		valueEnd, ok := scanJSONValueEnd(data, valueStart)
		if !ok {
			return nil, false
		}
		fields[key] = json.RawMessage(data[valueStart:valueEnd])
		i = skipJSONWhitespace(data, valueEnd)
		if i >= len(data) {
			return nil, false
		}
		if data[i] == '}' {
			return fields, i == len(data)-1
		}
		if data[i] != ',' {
			return nil, false
		}
		i = skipJSONWhitespace(data, i+1)
	}
	return nil, false
}

func scanJSONArrayElements(raw []byte, visit func([]byte) error) (bool, error) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || data[0] != '[' {
		return false, nil
	}
	i := skipJSONWhitespace(data, 1)
	if i < len(data) && data[i] == ']' {
		return i == len(data)-1, nil
	}
	for i < len(data) {
		valueStart := skipJSONWhitespace(data, i)
		valueEnd, ok := scanJSONValueEnd(data, valueStart)
		if !ok {
			return false, nil
		}
		if err := visit(data[valueStart:valueEnd]); err != nil {
			return true, err
		}
		i = skipJSONWhitespace(data, valueEnd)
		if i >= len(data) {
			return false, nil
		}
		if data[i] == ']' {
			return i == len(data)-1, nil
		}
		if data[i] != ',' {
			return false, nil
		}
		i++
	}
	return false, nil
}

func scanJSONStringEnd(data []byte, start int) (int, bool) {
	if start >= len(data) || data[start] != '"' {
		return 0, false
	}
	for i := start + 1; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
			if i >= len(data) {
				return 0, false
			}
		case '"':
			return i + 1, true
		default:
			if data[i] < 0x20 {
				return 0, false
			}
		}
	}
	return 0, false
}

func scanJSONValueEnd(data []byte, start int) (int, bool) {
	if start >= len(data) {
		return 0, false
	}
	switch data[start] {
	case '"':
		return scanJSONStringEnd(data, start)
	case '{', '[':
		depth := 0
		for i := start; i < len(data); {
			switch data[i] {
			case '"':
				end, ok := scanJSONStringEnd(data, i)
				if !ok {
					return 0, false
				}
				i = end
			case '{', '[':
				depth++
				i++
			case '}', ']':
				depth--
				i++
				if depth == 0 {
					return i, true
				}
			default:
				i++
			}
		}
		return 0, false
	default:
		for i := start; i < len(data); i++ {
			switch data[i] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return i, i > start
			}
		}
		return len(data), len(data) > start
	}
}

func rewriteDefaults(defaults map[string]json.RawMessage, assetBaseURL string) (map[string]json.RawMessage, bool, error) {
	if len(defaults) == 0 {
		return nil, false, nil
	}
	out := make(map[string]json.RawMessage, len(defaults))
	touched := false
	for path, value := range defaults {
		rewritten, valueTouched, err := rewriteJSONValidatedParent(value, assetBaseURL)
		if err != nil {
			return nil, false, fmt.Errorf("default %q: %w", path, err)
		}
		out[path] = rewritten
		touched = touched || valueTouched
	}
	return out, touched, nil
}

func rewriteAssets(raw json.RawMessage, assetBaseURL string, assetsTouched *bool) (json.RawMessage, error) {
	if len(raw) == 0 {
		if !*assetsTouched {
			return nil, nil
		}
		return addAllowedHost([]byte(`{}`), assetBaseURL)
	}
	rewritten, touched, err := rewriteJSONValidatedParent(raw, assetBaseURL)
	if err != nil {
		return nil, fmt.Errorf("assets: %w", err)
	}
	if touched {
		*assetsTouched = true
	}
	if !*assetsTouched {
		return rewritten, nil
	}
	return addAllowedHost(rewritten, assetBaseURL)
}

func addAllowedHost(raw json.RawMessage, assetBaseURL string) (json.RawMessage, error) {
	parsed, err := url.Parse(assetBaseURL)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("static bundle asset references require an absolute asset base URL")
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("assets must be a JSON object")
	}
	var hosts []any
	if existing, ok := object["allowedHosts"].([]any); ok {
		hosts = existing
	}
	for _, existing := range hosts {
		if existing == parsed.Host {
			return json.Marshal(object)
		}
	}
	object["allowedHosts"] = append(hosts, parsed.Host)
	return json.Marshal(object)
}

func rewriteJSON(raw json.RawMessage, assetBaseURL string) (json.RawMessage, bool, error) {
	return rewriteJSONFragment(raw, assetBaseURL, false)
}

// rewriteJSONValidatedParent is for fragments taken from a document that has
// already passed json.Unmarshal. It preserves rewriteJSON's defensive contract
// while avoiding a second full validity scan on large untouched fragments.
func rewriteJSONValidatedParent(raw json.RawMessage, assetBaseURL string) (json.RawMessage, bool, error) {
	return rewriteJSONFragment(raw, assetBaseURL, true)
}

func rewriteJSONFragment(raw json.RawMessage, assetBaseURL string, parentValidated bool) (json.RawMessage, bool, error) {
	if len(raw) == 0 {
		return raw, false, nil
	}
	// Most static properties and defaults contain neither asset references nor
	// JSON escapes. Keep those fragments as-is rather than materializing a
	// generic Go value and marshaling it back. Any escape forces the established
	// path so escaped spellings of asset references are still recognized.
	if bytes.IndexByte(raw, '\\') < 0 && !bytes.Contains(raw, []byte("assets/")) {
		if parentValidated || json.Valid(raw) {
			return raw, false, nil
		}
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, err
	}
	rewritten, touched, err := rewriteJSONValue(value, assetBaseURL)
	if err != nil {
		return nil, false, err
	}
	encoded, err := json.Marshal(rewritten)
	return encoded, touched, err
}

func rewriteJSONValue(value any, assetBaseURL string) (any, bool, error) {
	switch current := value.(type) {
	case string:
		match := staticAssetRef.FindStringSubmatch(current)
		if len(match) == 0 {
			return current, false, nil
		}
		parsed, err := url.Parse(assetBaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, false, errors.New("static bundle asset references require an absolute asset base URL")
		}
		return strings.TrimRight(assetBaseURL, "/") + "/" + strings.ToLower(match[1]) + "/bytes", true, nil
	case []any:
		changed := false
		for i, item := range current {
			rewritten, touched, err := rewriteJSONValue(item, assetBaseURL)
			if err != nil {
				return nil, false, err
			}
			current[i] = rewritten
			changed = changed || touched
		}
		return current, changed, nil
	case map[string]any:
		changed := false
		for key, item := range current {
			rewritten, touched, err := rewriteJSONValue(item, assetBaseURL)
			if err != nil {
				return nil, false, err
			}
			current[key] = rewritten
			changed = changed || touched
		}
		return current, changed, nil
	default:
		return value, false, nil
	}
}
