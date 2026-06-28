package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// PreviewWire is the persistent LSDP/1.1 wire dedicated to the cockpit
// preview — a SECOND wire beside the antenne's /show/stream.lsdp. The cockpit
// Solar connects to it ONCE (a fixed URL); switching the previewed scene swaps
// the wire's active clone via SetActive (the proven antenne switch path —
// scene_changed + fresh snapshot over the EXISTING socket, no client reload),
// so a preview switch never reconnects and never touches the antenne. The
// scenes mirrored here are PREVIEW CLONES, fully isolated from the global show.
// *lsdp.Wire satisfies this interface (MirrorFor/SetActive/Drop already exist);
// the interface lives in runtime to keep the import direction lsdp → runtime.
type PreviewWire interface {
	MirrorFor(sceneID, sceneVersion string, bundle *compiler.RenderBundle) SceneMirror
	SetActive(sceneID string)
	Drop(sceneID string)
}

// PreviewSlot owns the single live preview clone behind the persistent preview
// wire. The cockpit previews ONE scene at a time; Activate swaps the clone (a
// fresh isolated exec instance, never shared with the antenne show) and flips
// the preview wire to it. The antenne (global show + /show/stream.lsdp) is
// never touched — that structural separation is the whole point of the
// preview/antenne split: a preview scene switch can no longer flip the live
// antenne. nil in bespoke mode (no preview wire) — the preview routes degrade.
type PreviewSlot struct {
	registry *ComputeRegistry
	logger   *slog.Logger
	wire     PreviewWire

	mu      sync.Mutex
	ctx     context.Context
	current *previewClone
}

type previewClone struct {
	sceneID string
	scene   *Scene
}

// NewPreviewSlot builds the slot over a persistent preview wire. ctx is the
// process context: every preview clone runs on it, so a process shutdown stops
// the live preview with the rest of the runtime.
func NewPreviewSlot(ctx context.Context, registry *ComputeRegistry, wire PreviewWire, logger *slog.Logger) *PreviewSlot {
	return &PreviewSlot{
		registry: registry,
		logger:   logger.With("component", "preview-slot"),
		wire:     wire,
		ctx:      ctx,
	}
}

// Activate swaps the preview to a fresh isolated clone of sceneID and flips the
// preview wire to it. graph+bundle are the loaded scene's compiled artefacts —
// copied so the preview exec never shares state with the antenne instance.
// progs installs the scene's full exec program set UNGATED (like a test
// session: the preview is author-facing, exec runs without the R9 air gate).
// Re-activating the same scene rebuilds a fresh clone (reseeds defaults +
// fires on-start), matching a push-swap of the live scene on the antenne.
func (p *PreviewSlot) Activate(sceneID string, graph *compiler.Graph, bundle *compiler.RenderBundle, progs ...*ExecProgram) {
	p.mu.Lock()
	defer p.mu.Unlock()

	prev := p.current

	// Clone the compiled artefacts so the preview's reactive loop owns private
	// inputs — the antenne instance of the same scene is untouched.
	gcopy := *graph
	bcopy := *bundle
	scene := NewScene(sceneID, &gcopy, &bcopy, p.registry, p.logger.With("preview_scene", sceneID))
	scene.InstallExec(progs...)
	// Pair with the PREVIEW wire before Run (SetMirror seeds the kit scene with
	// the clone's snapshot). MirrorFor registers the kit scene under sceneID on
	// the preview wire ONLY — never the antenne wire (a different Server).
	scene.SetMirror(p.wire.MirrorFor(sceneID, gcopy.SceneVersion, &bcopy))
	go scene.Run(p.ctx)
	scene.FireOnStart("system:preview-activated")
	// Flip the preview wire's live endpoint to this clone: the connected
	// preview Solar migrates (scene_changed + fresh snapshot) over its EXISTING
	// socket — no reload. Identical mechanism to the antenne's Show.SetActive.
	p.wire.SetActive(sceneID)
	p.current = &previewClone{sceneID: sceneID, scene: scene}

	// Tear the previous clone down AFTER the flip (no preview gap). Keep the
	// kit scene if we just re-activated the same id — MirrorFor re-registered
	// it, so dropping would orphan the fresh clone.
	if prev != nil {
		prev.scene.Stop()
		if prev.sceneID != sceneID {
			p.wire.Drop(prev.sceneID)
		}
	}
}

// SnapshotState exports the live preview clone's state for the preview→air
// hand-off (the antenne show no longer runs the preview scene, so the export
// must read it here — the show would return blank/stale defaults). Read-only.
// ok is false when no scene has been previewed yet this run.
func (p *PreviewSlot) SnapshotState() (version string, seq uint64, state map[string]json.RawMessage, ok bool) {
	p.mu.Lock()
	cur := p.current
	p.mu.Unlock()
	if cur == nil {
		return "", 0, nil, false
	}
	version, seq, state = cur.scene.SnapshotState()
	return version, seq, state, true
}

// CurrentSceneID is the scene currently in the preview slot, or "" when none.
func (p *PreviewSlot) CurrentSceneID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return ""
	}
	return p.current.sceneID
}

// Current is the live preview clone scene, or nil when none is open. It is the
// operator-surface target in preview mode: firing an on-call / resolving an
// await against the preview clone drives the PREVIEW, never the antenne's
// active scene (the preview/antenne split applied to the operator routes).
func (p *PreviewSlot) Current() *Scene {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return nil
	}
	return p.current.scene
}

// Close stops the live preview clone — called at process shutdown.
func (p *PreviewSlot) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current != nil {
		p.current.scene.Stop()
		p.wire.Drop(p.current.sceneID)
		p.current = nil
	}
}
