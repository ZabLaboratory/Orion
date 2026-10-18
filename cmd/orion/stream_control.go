package main

import (
	"fmt"
	"path/filepath"

	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/streamcontrol"
)

func wireStreamControl(cfg config.Config, mirror streamcontrol.Mirror) (*streamcontrol.Store, *streamcontrol.Controller, func(), error) {
	path := cfg.StreamIntentPath
	if path == "" {
		root := cfg.LocalArtifactRoot
		if cfg.ServiceTokenStatePath != "" {
			root = filepath.Dir(cfg.ServiceTokenStatePath)
		}
		if root == "" {
			root = cfg.AssetRoot
		}
		path = filepath.Join(root, "stream-control.lsml")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, nil, err
	}
	store, err := streamcontrol.Open(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stream intent: %w", err)
	}
	apps, err := streamcontrol.NewApps(cfg.OverlayAppsPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("overlay applications: %w", err)
	}
	controller := streamcontrol.NewController(store, apps, mirror)
	return store, controller, func() { apps.Close() }, nil
}

type streamMirrors struct {
	runtime.MirrorRegistry
	controller *streamcontrol.Controller
}

func (m streamMirrors) EmitOverlayApp(id string, running, onAir *bool) {
	m.controller.EmitOverlayApp(id, running, onAir)
}
