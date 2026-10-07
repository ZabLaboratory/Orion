package lsdp

import "log/slog"

// RenderSurface shares the authored binding contract with native reception.
// Native JSON permits object rows, so only the binding gate is reused; the
// retired LSDP/1 scalar restriction does not apply to native document values.
func RenderSurface(sceneID string, source []byte, logger *slog.Logger) func(string) bool {
	bound := boundLeavesFromLSML(sceneID, source, logger)
	return func(path string) bool { return !bound.active() || bound.renderable(path) }
}
