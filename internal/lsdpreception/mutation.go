package lsdpreception

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/ZabLaboratory/Orion/internal/lsdp"
)

// ApplyLSML changes only this mirror's scene, preserving the attested identity.
// It shares the producer's atomic native transaction and never writes source files.
func (m *Mirror) ApplyLSML(operations []map[string]any) error {
	if len(operations) == 0 || len(operations) > 128 {
		return errors.New("LSML_MUTATION_LIMIT")
	}
	for _, operation := range operations {
		path, _ := operation["path"].(string)
		allowed := false
		for _, root := range []string{"/layout", "/defaults", "/animations"} {
			if path == root || strings.HasPrefix(path, root+"/") {
				allowed = true
			}
		}
		if !allowed || path == "/defaults" || strings.HasPrefix(path, "/defaults/__cam.") {
			return errors.New("LSML_MUTATION_PATH_FORBIDDEN")
		}
	}
	h := m.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if m.lane != nil && m.lane.transitioning {
		return errors.New("LSML_SCENE_TRANSITION_IN_PROGRESS")
	}
	if m.document == nil || (m.lane != nil && m.lane.scenes[m.sceneID] != m) || (m.lane == nil && !m.session && h.generations[m.key] != m) {
		return errors.New("LSML_MUTATION_STALE_GENERATION")
	}
	next, err := applyObjectOperations(m.document, operations)
	if err != nil {
		return err
	}
	document := next.(map[string]any)
	if _, ok := document["layout"].(map[string]any); !ok {
		return errors.New("LSML_MUTATION_LAYOUT_REQUIRED")
	}
	if _, ok := document["defaults"].(map[string]any); !ok {
		return errors.New("LSML_MUTATION_DEFAULTS_REQUIRED")
	}
	if err := validateAnimationCommands(document); err != nil {
		return err
	}
	data, err := json.Marshal(document)
	if err != nil || len(data) > 16<<20 {
		return errors.New("LSML_MUTATION_DOCUMENT_LIMIT")
	}
	m.document = document
	m.surface = lsdp.RenderSurface(m.sceneID, data, h.logger)
	target := "solar/generations"
	if m.lane != nil {
		if m.lane.active == m {
			h.producer.Apply(m.lane.target, operations)
		}
		return nil
	}
	if m.session {
		target = "solar/sessions"
	}
	scoped := make([]map[string]any, 0, len(operations))
	for _, operation := range operations {
		scopedOperation := map[string]any{}
		for k, v := range operation {
			scopedOperation[k] = v
		}
		scopedOperation["path"] = "/" + pointer(m.key) + operation["path"].(string)
		scoped = append(scoped, scopedOperation)
	}
	h.producer.Apply(target, scoped)
	return nil
}

// Reject an undeclared command before it can poison a subscribed scene.
func validateAnimationCommands(document map[string]any) error {
	layout, _ := document["layout"].(map[string]any)
	catalogue, _ := document["animations"].(map[string]any)
	if catalogue == nil {
		catalogue, _ = layout["animations"].(map[string]any)
	}
	var hasTarget func(any, string) bool
	hasTarget = func(value any, target string) bool {
		switch node := value.(type) {
		case map[string]any:
			if node["id"] == target {
				return true
			}
			return hasTarget(node["children"], target) || hasTarget(node["template"], target)
		case []any:
			for _, child := range node {
				if hasTarget(child, target) {
					return true
				}
			}
		}
		return false
	}
	defaults, _ := document["defaults"].(map[string]any)
	for key, raw := range defaults {
		if !strings.HasPrefix(key, "__animation.") {
			continue
		}
		command, _ := raw.(map[string]any)
		id, _ := command["animation_id"].(string)
		asset, _ := catalogue[id].(map[string]any)
		target, _ := asset["target"].(string)
		if id == "" || target == "" || !hasTarget(layout, target) {
			return errors.New("ANIMATION_ASSET_OR_TARGET_UNDECLARED")
		}
	}
	return nil
}

func patchNode(node any, parts []string, operation map[string]any) (any, error) {
	if len(parts) == 0 {
		switch operation["op"] {
		case "add", "replace":
			return operation["value"], nil
		case "test":
			if reflect.DeepEqual(node, operation["value"]) {
				return node, nil
			}
			return nil, errors.New("NATIVE_PATCH_TEST_FAILED")
		default:
			return nil, errors.New("NATIVE_PATCH_OPERATION_UNSUPPORTED")
		}
	}
	key := parts[0]
	switch current := node.(type) {
	case map[string]any:
		value, exists := current[key]
		if len(parts) == 1 {
			switch operation["op"] {
			case "add":
				current[key] = operation["value"]
			case "replace":
				if !exists {
					return nil, errors.New("NATIVE_PATCH_LEAF_MISSING")
				}
				current[key] = operation["value"]
			case "remove":
				if !exists {
					return nil, errors.New("NATIVE_PATCH_LEAF_MISSING")
				}
				delete(current, key)
			case "test":
				if !exists || !reflect.DeepEqual(value, operation["value"]) {
					return nil, errors.New("NATIVE_PATCH_TEST_FAILED")
				}
			default:
				return nil, errors.New("NATIVE_PATCH_OPERATION_UNSUPPORTED")
			}
			return current, nil
		}
		if !exists {
			return nil, errors.New("NATIVE_PATCH_PARENT_MISSING")
		}
		next, err := patchNode(value, parts[1:], operation)
		if err != nil {
			return nil, err
		}
		current[key] = next
		return current, nil
	case []any:
		index, err := strconv.Atoi(key)
		if key == "-" && len(parts) == 1 && operation["op"] == "add" {
			index = len(current)
			err = nil
		}
		if err != nil || index < 0 || (key != "-" && strconv.Itoa(index) != key) || index > len(current) {
			return nil, errors.New("NATIVE_PATCH_ARRAY_INDEX")
		}
		if len(parts) == 1 && operation["op"] == "add" {
			current = append(current, nil)
			copy(current[index+1:], current[index:])
			current[index] = operation["value"]
			return current, nil
		}
		if index >= len(current) {
			return nil, errors.New("NATIVE_PATCH_ARRAY_INDEX")
		}
		if len(parts) == 1 && operation["op"] == "remove" {
			return append(current[:index], current[index+1:]...), nil
		}
		next, err := patchNode(current[index], parts[1:], operation)
		if err != nil {
			return nil, err
		}
		current[index] = next
		return current, nil
	default:
		return nil, fmt.Errorf("NATIVE_PATCH_PARENT_TYPE: %T", node)
	}
}
