package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/ZabLaboratory/Orion/internal/api"
	"github.com/ZabLaboratory/Orion/internal/auth"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"net/http"
	"path/filepath"
)

func wireSceneControl(cfg config.Config, deps *api.SceneIntentDeps, reception *lsdpreception.Reception) (*api.SceneControl, error) {
	if deps == nil {
		return nil, fmt.Errorf("scene control requires signed scene admission")
	}
	// Orion owns this subtree. It never writes Prism's immutable artifact cache.
	root := ""
	switch {
	case cfg.StreamIntentPath != "":
		root = filepath.Dir(cfg.StreamIntentPath)
	case cfg.ServiceTokenStatePath != "":
		root = filepath.Dir(cfg.ServiceTokenStatePath)
	default:
		root = cfg.AssetRoot
	}
	identity := sha256.Sum256([]byte(cfg.OwnerID + "\x00" + cfg.TenantID + "\x00" + cfg.LocalAuthUser))
	root = filepath.Join(root, "orion-scenes", hex.EncodeToString(identity[:]))
	path := cfg.SceneControlPath
	if path == "" {
		path = filepath.Join(root, "selection.lsml")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cache := cfg.SceneCachePath
	if cache == "" {
		cache = filepath.Join(root, "catalog")
	}
	cache, err = filepath.Abs(cache)
	if err != nil {
		return nil, err
	}
	deps.Catalog, err = api.OpenSceneCatalog(cache)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set(auth.HandshakeHeader, cfg.LocalAuthSecret)
	return api.OpenSceneControl(*deps, reception, path, header)
}
