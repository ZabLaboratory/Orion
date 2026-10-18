// Package lsml implements the LSML 1.0 scene format : type definitions,
// validation, and content-hash canonicalisation.
//
// LSML bundles are JSON documents whose top-level shape and primitive
// catalog are documented at
// https://github.com/Lumencast/lumencast-protocol/blob/main/spec/LSML-1.md.
package lsml

import "encoding/json"

// Version is the LSML major.minor this package implements.
const Version = "1.0"

// Bundle is the top-level scene document.
type Bundle struct {
	LSML             string            `json:"lsml"`
	SceneID          string            `json:"scene_id"`
	SceneVersion     string            `json:"scene_version"`
	Layout           json.RawMessage   `json:"layout"`
	OperatorInputs   []OperatorInput   `json:"operator_inputs,omitempty"`
	ExternalAdapters []json.RawMessage `json:"external_adapters,omitempty"`
	Defaults         map[string]any    `json:"defaults,omitempty"`
	Assets           *Assets           `json:"assets,omitempty"`
	I18n             json.RawMessage   `json:"i18n,omitempty"`
	Metadata         json.RawMessage   `json:"metadata,omitempty"`
}

// OperatorInput describes a single operator-controllable field.
type OperatorInput struct {
	Path        string         `json:"path"`
	Label       string         `json:"label"`
	Type        string         `json:"type"`
	Constraints map[string]any `json:"constraints,omitempty"`
	Values      []any          `json:"values,omitempty"`
	WritableBy  []string       `json:"writable_by"`
	Group       string         `json:"group,omitempty"`
}

// Assets is the asset declaration block.
type Assets struct {
	AllowedHosts []string         `json:"allowedHosts,omitempty"`
	Fonts        []FontAsset      `json:"fonts,omitempty"`
	Preload      []string         `json:"preload,omitempty"`
	Extra        map[string]any   `json:"-"`
}

// FontAsset is one font declaration.
type FontAsset struct {
	Family string `json:"family"`
	URL    string `json:"url"`
	Sha256 string `json:"sha256,omitempty"`
}

// Node is the discriminated representation of a primitive node. Most
// validation walks the raw JSON tree directly ; Node is convenience
// for code that wants typed access.
type Node struct {
	Kind     string          `json:"kind"`
	Children []Node          `json:"children,omitempty"`
	Bind     json.RawMessage `json:"bind,omitempty"`
	Style    json.RawMessage `json:"style,omitempty"`
	Animate  json.RawMessage `json:"animate,omitempty"`
	Extra    json.RawMessage `json:"-"`
}
