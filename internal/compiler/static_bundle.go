package compiler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
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
		return nil, nil, fmt.Errorf("decode static LSML bundle: %w", err)
	}
	// Accept an already compiled bundle for forward compatibility. The
	// current ZabCanvas producer sends layout; this branch avoids a needless
	// second lowering if a future producer sends root directly.
	if len(source.Layout) == 0 && len(source.Root) != 0 {
		defaults, _, err := rewriteDefaults(source.Defaults, assetBaseURL)
		if err != nil {
			return nil, nil, err
		}
		return raw, defaults, nil
	}
	if len(source.Layout) == 0 {
		return nil, nil, errors.New("static LSML bundle has neither layout nor compiled root")
	}

	var rawRoot map[string]json.RawMessage
	if err := json.Unmarshal(source.Layout, &rawRoot); err != nil || rawRoot == nil {
		if err == nil {
			err = errors.New("layout is not an object")
		}
		return nil, nil, fmt.Errorf("decode static LSML layout: %w", err)
	}

	assetsTouched := false
	root, err := adaptStaticNode(rawRoot, assetBaseURL, &assetsTouched)
	if err != nil {
		return nil, nil, fmt.Errorf("adapt static LSML layout: %w", err)
	}
	animations := source.Animations
	if len(animations) == 0 {
		animations = rawRoot["animations"]
	}
	loweredRoot := lowerRenderTree(root, parseAnimationCatalogue(animations))

	defaults, defaultsTouched, err := rewriteDefaults(source.Defaults, assetBaseURL)
	if err != nil {
		return nil, nil, err
	}
	assetsTouched = assetsTouched || defaultsTouched
	assets, err := rewriteAssets(source.Assets, assetBaseURL, &assetsTouched)
	if err != nil {
		return nil, nil, err
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
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return nil, nil, fmt.Errorf("encode static RenderBundle: %w", err)
	}
	return encoded, defaults, nil
}

var staticAssetRef = regexp.MustCompile(`^assets/([0-9a-fA-F]{64})(?:\.[A-Za-z0-9]+)?$`)

func adaptStaticNode(raw map[string]json.RawMessage, assetBaseURL string, assetsTouched *bool) (LayoutNode, error) {
	var node LayoutNode
	if err := json.Unmarshal(raw["kind"], &node.Kind); err != nil || node.Kind == "" {
		if err == nil {
			err = errors.New("node kind is empty")
		}
		return LayoutNode{}, err
	}
	_ = json.Unmarshal(raw["id"], &node.ID)

	for key, value := range raw {
		switch key {
		case "kind", "id", "children", "animate", "animations", "bind", "bindStyle", "bindUniversal":
			continue
		default:
			rewritten, touched, err := rewriteJSON(value, assetBaseURL)
			if err != nil {
				return LayoutNode{}, fmt.Errorf("property %q: %w", key, err)
			}
			if touched {
				*assetsTouched = true
			}
			if node.Props == nil {
				node.Props = make(map[string]json.RawMessage)
			}
			node.Props[key] = rewritten
		}
	}

	for _, bindingKey := range []string{"bind", "bindStyle", "bindUniversal"} {
		var bindings map[string]string
		if err := json.Unmarshal(raw[bindingKey], &bindings); err != nil {
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
	if err := json.Unmarshal(raw["animate"], &node.Transitions); err != nil {
		node.Transitions = nil
	}

	var children []json.RawMessage
	if err := json.Unmarshal(raw["children"], &children); err == nil {
		for _, childRaw := range children {
			var child map[string]json.RawMessage
			if err := json.Unmarshal(childRaw, &child); err != nil || child == nil {
				continue
			}
			childNode, err := adaptStaticNode(child, assetBaseURL, assetsTouched)
			if err != nil {
				return LayoutNode{}, err
			}
			node.Children = append(node.Children, childNode)
		}
	}
	return node, nil
}

func rewriteDefaults(defaults map[string]json.RawMessage, assetBaseURL string) (map[string]json.RawMessage, bool, error) {
	if len(defaults) == 0 {
		return nil, false, nil
	}
	out := make(map[string]json.RawMessage, len(defaults))
	touched := false
	for path, value := range defaults {
		rewritten, valueTouched, err := rewriteJSON(value, assetBaseURL)
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
	rewritten, touched, err := rewriteJSON(raw, assetBaseURL)
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
	if len(raw) == 0 {
		return raw, false, nil
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
