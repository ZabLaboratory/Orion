package api

import (
	"encoding/json"
)

// prepareCurrentRender extracts an explicitly attached, attested compatibility
// capsule. Scene-intent switches never compile LSML: Solar fetches the pinned
// source from ZabCanvas and Vision renders it directly. The compiler remains
// available to explicit validation and editable-preview routes.
func prepareCurrentRender(artifacts verifiedSceneArtifacts) ([]byte, map[string]json.RawMessage, error) {
	if artifacts.CurrentRender != nil {
		defaults, err := decodeRenderBundleDefaults(artifacts.CurrentRender)
		return artifacts.CurrentRender, defaults, err
	}
	return nil, nil, nil
}
