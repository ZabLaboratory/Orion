package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// Local authoring verifies the immutable LSML address without lowering its tree.
// The private Preview clone still owns sequencing/defaults; no Blue admission is created.
func editableSourceBundle(body editablePreviewOpenRequest) (*compiler.RenderBundle, map[string]json.RawMessage, error) {
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body.LSMLBundle))
	decoder.UseNumber()
	if decoder.Decode(&document) != nil || document == nil {
		return nil, nil, errors.New("invalid editable LSML document")
	}
	version := document["lsml"]
	_, layout := document["layout"].(map[string]any)
	if (version != "1.0" && version != "1.1" && version != "1.2") || !layout || document["scene_id"] != body.SceneID || document["scene_version"] != body.SceneVersion {
		return nil, nil, errors.New("editable LSML identity or layout mismatch")
	}
	document["scene_version"] = "sha256:" + strings.Repeat("0", 64)
	digest, err := canonical.Digest(document)
	if err != nil || digest != body.SceneVersion {
		return nil, nil, errors.New("editable LSML content address mismatch")
	}
	var source struct {
		Defaults map[string]json.RawMessage `json:"defaults"`
	}
	if err := json.Unmarshal(body.LSMLBundle, &source); err != nil {
		return nil, nil, err
	}
	return &compiler.RenderBundle{SceneVersion: body.SceneVersion, Defaults: source.Defaults, SourceLSML: rawBundleCopy(body.LSMLBundle)}, source.Defaults, nil
}

// Native authoring delivery shares the scene's LSDP resource. The immutable
// base remains addressable while live defaults evolve through native mutations.
func editableNativeSource(body editablePreviewOpenRequest) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body.LSMLBundle, &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, errors.New("editable LSML object required")
	}
	if _, exists := document["x-solar-authoring"]; exists {
		return nil, errors.New("nested editable authoring delivery is forbidden")
	}
	assets := map[string][]byte{}
	fonts := []string{}
	fontPath := regexp.MustCompile(`^assets/([a-f0-9]{64})\.(ttf|otf|ttc|woff|woff2)$`)
	for path, bytes := range body.Assets {
		if match := fontPath.FindStringSubmatch(path); match != nil {
			digest := sha256.Sum256(bytes)
			if hex.EncodeToString(digest[:]) != match[1] {
				return nil, errors.New("editable font content address mismatch")
			}
			fonts = append(fonts, match[1])
		} else {
			assets[path] = bytes
		}
	}
	sort.Strings(fonts)
	envelope, err := json.Marshal(map[string]any{
		"format":      "solar.authoring/1",
		"lsml_bundle": json.RawMessage(body.LSMLBundle),
		"assets":      assets,
		"fonts":       fonts,
	})
	if err != nil {
		return nil, err
	}
	document["x-solar-authoring"] = envelope
	return json.Marshal(document)
}
