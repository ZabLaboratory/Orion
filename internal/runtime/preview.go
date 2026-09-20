package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/protocol"
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

// EditableAirWire is the immutable generation wire registry used when an
// editable Preview clone is explicitly armed for a Pulsar lane. It is kept as
// a small runtime interface so PreviewSlot does not import the LSDP package or
// accidentally couple the Preview slot to the Program/antenne wire.
type EditableAirWire interface {
	MirrorForLSML(sceneID, sceneVersion, owner string, lsmlBundle []byte) SceneMirror
	SetActive(sceneID, sceneVersion string)
}

// sceneMirrorFanout is installed at clone creation time. Editable patches
// therefore continue to use the same scene goroutine and can add the explicit
// on-air generation mirror later without mutating Scene.SetMirror while the
// scene is running.
type sceneMirrorFanout struct {
	mu      sync.RWMutex
	mirrors []SceneMirror
}

func newSceneMirrorFanout(primary SceneMirror) *sceneMirrorFanout {
	if primary == nil {
		return &sceneMirrorFanout{}
	}
	return &sceneMirrorFanout{mirrors: []SceneMirror{primary}}
}

func (f *sceneMirrorFanout) Add(mirror SceneMirror) {
	if mirror == nil {
		return
	}
	f.mu.Lock()
	f.mirrors = append(f.mirrors, mirror)
	f.mu.Unlock()
}

func (f *sceneMirrorFanout) Forward(message SubscriberMsg) {
	f.mu.RLock()
	mirrors := append([]SceneMirror(nil), f.mirrors...)
	f.mu.RUnlock()
	for _, mirror := range mirrors {
		mirror.Forward(message)
	}
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
	// editable keeps already-compiled no-Blue clones warm while a regular
	// scene temporarily owns the Preview wire. Re-selecting an editable scene
	// is therefore only a Wire.SetActive operation: no LSML compilation, no
	// clone restart and no Solar/Pulsar reconnection.
	editable map[string]*previewClone
	effects  *SceneEffects
	// editableAir is deliberately separate from the persistent Preview wire.
	// A promotion adds a generation mirror to the clone's fan-out; it never
	// reuses or retargets the Program/antenne wire.
	editableAir EditableAirWire
}

type previewClone struct {
	sceneID      string
	sceneVersion string
	scene        *Scene
	bundle       []byte
	lsmlBundle   []byte
	mirror       *sceneMirrorFanout
	editable     bool
	editSeq      uint64
	airPromoted  bool
}

var (
	ErrPreviewNotEditable    = errors.New("preview scene is not editable")
	ErrPreviewSceneMismatch  = errors.New("editable preview scene mismatch")
	ErrPreviewEditSequence   = errors.New("editable preview sequence conflict")
	ErrPreviewEditPath       = errors.New("editable preview path is not declared")
	ErrPreviewBusy           = errors.New("editable preview input queue is full")
	ErrPreviewCacheMiss      = errors.New("editable preview clone is not cached")
	ErrPreviewAirUnavailable = errors.New("editable preview air wire is unavailable")
)

type EditablePatch struct {
	Path  string
	Value json.RawMessage
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
		editable: make(map[string]*previewClone),
	}
}

// SetEffects installs the shared async-effect executor bundle on the slot, so
// every preview clone that carries exec programs can run WORLD-EFFECT ops
// (db.query, http.request, source.read, animation) — exactly like the antenne
// show (cmd/orion/main.go: show.SetEffects). Without it a clone's exec hits
// "unregistered exec op" on the first db.query and the on-call chain dies
// silently (the LCK/LEC button fires but nothing changes). The bundle is shared
// read-only across instances. Called once at boot, before any Activate.
func (p *PreviewSlot) SetEffects(e *SceneEffects) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.effects = e
}

// SetEditableAirWire installs the immutable generation registry used by an
// explicit editable Preview → Pulsar lane hand-off. It is called once during
// Orion boot, after both LSDP registries exist.
func (p *PreviewSlot) SetEditableAirWire(wire EditableAirWire) {
	p.mu.Lock()
	p.editableAir = wire
	p.mu.Unlock()
}

// Activate swaps the preview to a fresh isolated clone of sceneID and flips the
// preview wire to it. graph+bundle are the loaded scene's compiled artefacts —
// copied so the preview exec never shares state with the antenne instance.
// progs installs the scene's full exec program set UNGATED (like a test
// session: the preview is author-facing, exec runs without the R9 air gate).
// Re-activating the same scene rebuilds a fresh clone (reseeds defaults +
// fires on-start), matching a push-swap of the live scene on the antenne.
func (p *PreviewSlot) Activate(sceneID string, graph *compiler.Graph, bundle *compiler.RenderBundle, progs ...*ExecProgram) {
	p.activate(sceneID, graph, bundle, false, 0, nil, progs...)
}

// ActivateStatic installs a no-Blue render bundle in the editable preview
// lane. It is the local cockpit convenience path: the bundle already carries
// its declared defaults, so Orion can construct the bounded graph without
// invoking a Blue runtime. The scene starts at edit sequence zero.
func (p *PreviewSlot) ActivateStatic(sceneID string, bundle *compiler.RenderBundle) error {
	if sceneID == "" || bundle == nil {
		return ErrPreviewSceneMismatch
	}
	graph := &compiler.Graph{
		SceneID:        sceneID,
		SceneVersion:   bundle.SceneVersion,
		Defaults:       bundle.Defaults,
		Bindings:       bundle.ExternalAdapters,
		OperatorInputs: bundle.OperatorInputs,
	}
	p.activate(sceneID, graph, bundle, true, 0, nil)
	return nil
}

func (p *PreviewSlot) activate(sceneID string, graph *compiler.Graph, bundle *compiler.RenderBundle, editable bool, editSeq uint64, lsmlBundle []byte, progs ...*ExecProgram) {
	p.mu.Lock()
	defer p.mu.Unlock()

	prev := p.current
	replaced := p.editable[sceneID]

	// Clone the compiled artefacts so the preview's reactive loop owns private
	// inputs — the antenne instance of the same scene is untouched.
	gcopy := *graph
	bcopy := *bundle
	scene := NewScene(sceneID, &gcopy, &bcopy, p.registry, p.logger.With("preview_scene", sceneID))
	scene.InstallExec(progs...)
	// Install the world-effect ops (db.query, http.request, …) on the clone
	// when it carries exec programs — mirrors the show's LoadExec
	// (show.go: len(progs) > 0 && sh.effects != nil → scene.SetEffects). Without
	// this the on-call chain dies on the first db.query (unregistered exec op).
	// Pre-Run, like the show.
	if len(progs) > 0 {
		// Preview is a private world-effect policy. Clone the shared dependency
		// bundle so the preview cannot mutate the on-air policy, then mark this
		// clone synthetic for HTTP and service.call. The compiler-curated,
		// read-only db.query path still uses the shared gateway data service.
		previewEffects := SceneEffects{}
		if p.effects != nil {
			previewEffects = *p.effects
		}
		previewEffects.Preview = true
		scene.SetEffects(&previewEffects)
	}
	// Keep the preview's stream key isolated from the live show (ADR Blue 009
	// §B / R3). Synthetic preview effects do not spend the egress budget, but
	// the key remains stable for any non-world local effect accounting and for
	// a future policy transition. Two preview scene ids remain independent.
	scene.SetStreamKey("preview:" + sceneID)
	// Pair with the PREVIEW wire before Run (SetMirror seeds the kit scene with
	// the clone's snapshot). MirrorFor registers the kit scene under sceneID on
	// the preview wire ONLY — never the antenne wire (a different Server).
	mirror := newSceneMirrorFanout(p.wire.MirrorFor(sceneID, gcopy.SceneVersion, &bcopy))
	scene.SetMirror(mirror)
	go scene.Run(p.ctx)
	scene.FireOnStart("system:preview-activated")
	// Flip the preview wire's live endpoint to this clone: the connected
	// preview Solar migrates (scene_changed + fresh snapshot) over its EXISTING
	// socket — no reload. Identical mechanism to the antenne's Show.SetActive.
	p.wire.SetActive(sceneID)
	bundleBytes, err := json.Marshal(&bcopy)
	if err != nil {
		// RenderBundle is composed entirely of JSON-backed fields and was
		// already decoded before reaching the runtime. Keep activation live if
		// a future extension violates that assumption, but make the fetch gap
		// explicit instead of serving corrupt bytes.
		p.logger.Error("preview render bundle cannot be serialized", "scene_id", sceneID, "err", err)
		bundleBytes = nil
	}
	next := &previewClone{
		sceneID:      sceneID,
		sceneVersion: gcopy.SceneVersion,
		scene:        scene,
		bundle:       bundleBytes,
		lsmlBundle:   append([]byte(nil), lsmlBundle...),
		mirror:       mirror,
		editable:     editable,
		editSeq:      editSeq,
	}
	p.current = next
	if editable {
		p.editable[sceneID] = next
	} else {
		delete(p.editable, sceneID)
	}

	// Tear disposable clones down AFTER the flip (no preview gap). An editable
	// clone with another id deliberately stays alive in p.editable so a later
	// switch back is compilation-free. A same-id cached clone is replaced by
	// the fresh structural build above and must be stopped exactly once.
	if replaced != nil && replaced != next {
		replaced.scene.Stop()
	}
	if prev != nil && prev != replaced && !prev.editable {
		prev.scene.Stop()
		if prev.sceneID != sceneID {
			p.wire.Drop(prev.sceneID)
		}
	}
}

// Bundle returns the exact compiled render tree owned by the persistent
// Preview slot. Solar learns (scene_id, scene_version) from preview.lsdp and
// resolves that immutable pair through the ordinary render-bundle endpoint;
// bluehost does not own editable scenes, so this is the missing read side of
// their no-Blue activation path. A defensive copy prevents HTTP writers from
// aliasing the live preview clone.
func (p *PreviewSlot) Bundle(sceneID, sceneVersion string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.current
	if cur == nil || cur.sceneID != sceneID {
		cur = p.editable[sceneID]
	}
	if cur == nil || cur.sceneVersion != sceneVersion || len(cur.bundle) == 0 {
		return nil, false
	}
	return append([]byte(nil), cur.bundle...), true
}

// ActivateEditable arms a no-Blue preview clone and records the durable
// ZabCanvas edit sequence that subsequent hot patches must extend.
func (p *PreviewSlot) ActivateEditable(sceneID string, graph *compiler.Graph, bundle *compiler.RenderBundle, editSeq uint64) {
	p.activate(sceneID, graph, bundle, true, editSeq, nil)
}

// ActivateEditableWithBundle is the source-preserving variant used by the
// HTTP editable-preview route. Generation wires need the original LSML bytes
// to derive their bound leaf surface; the compiled RenderBundle alone is not
// a valid LSML bundle.
func (p *PreviewSlot) ActivateEditableWithBundle(sceneID string, graph *compiler.Graph, bundle *compiler.RenderBundle, editSeq uint64, lsmlBundle []byte) {
	p.activate(sceneID, graph, bundle, true, editSeq, lsmlBundle)
}

// PromoteEditable attaches the current no-Blue clone to its immutable
// generation wire and makes that generation addressable for a Pulsar lane.
// Preview remains active on its own persistent wire and later Preview scene
// switches cannot mutate the generation entry. Hot edits still fan out to both
// consumers, which is the intended editable-on-air behaviour.
func (p *PreviewSlot) PromoteEditable(sceneID, owner string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	clone := p.editable[sceneID]
	if clone == nil || !clone.editable {
		return "", ErrPreviewCacheMiss
	}
	if p.editableAir == nil || len(clone.lsmlBundle) == 0 {
		return "", ErrPreviewAirUnavailable
	}
	if clone.airPromoted {
		p.editableAir.SetActive(sceneID, clone.sceneVersion)
		return clone.sceneVersion, nil
	}
	mirror := p.editableAir.MirrorForLSML(sceneID, clone.sceneVersion, owner, clone.lsmlBundle)
	if mirror == nil {
		return "", ErrPreviewAirUnavailable
	}
	clone.mirror.Add(mirror)
	version, seq, state := clone.scene.SnapshotState()
	mirror.Forward(&protocol.Snapshot{
		SceneID:      sceneID,
		SceneVersion: version,
		Sequence:     seq,
		State:        state,
	})
	p.editableAir.SetActive(sceneID, version)
	clone.airPromoted = true
	return version, nil
}

// ReactivateEditable flips Preview back to an already-compiled editable
// clone. expectedEditSeq binds Prism's durable ZabCanvas head to the cached
// runtime instance so stale UI state can never silently reactivate. The clone
// and its scene mirror stay alive while Blue owns Preview; only the preview
// wire's active scene changes here.
func (p *PreviewSlot) ReactivateEditable(sceneID string, expectedEditSeq uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := p.editable[sceneID]
	if next == nil {
		return ErrPreviewCacheMiss
	}
	if next.editSeq != expectedEditSeq {
		return ErrPreviewEditSequence
	}
	prev := p.current
	if prev == next {
		// Blue scene-intent uses the same persistent Preview wire but is
		// intentionally owned by bluehost rather than PreviewSlot.  In that
		// path p.current still points at this warm clone while the wire has
		// moved to Blue, so returning early would acknowledge activation
		// without emitting the scene_changed/snapshot pair Solar needs.
		// Reassert the wire even when the cached clone is already current;
		// this is idempotent for an actually-active editable scene and closes
		// the external-owner hand-off gap without rebuilding the clone.
		p.wire.SetActive(sceneID)
		return nil
	}
	p.wire.SetActive(sceneID)
	p.current = next
	if prev != nil && !prev.editable {
		prev.scene.Stop()
		if prev.sceneID != sceneID {
			p.wire.Drop(prev.sceneID)
		}
	}
	return nil
}

// ApplyEditablePatches queues one atomic hot edit on the live preview clone.
// All values travel through the existing scene mirror, so Solar receives a
// real LSDP delta over its persistent socket; the antenna show is untouched.
func (p *PreviewSlot) ApplyEditablePatches(sceneID string, baseSeq, editSeq uint64, patches []EditablePatch) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.current
	if cur == nil || cur.sceneID != sceneID {
		return ErrPreviewSceneMismatch
	}
	if !cur.editable {
		return ErrPreviewNotEditable
	}
	if cur.editSeq != baseSeq || editSeq != baseSeq+1 {
		return ErrPreviewEditSequence
	}
	keyspace := cur.scene.DeclaredKeyspace()
	for _, patch := range patches {
		if _, ok := keyspace[patch.Path]; !ok {
			return ErrPreviewEditPath
		}
	}
	batch := append([]EditablePatch(nil), patches...)
	if !cur.scene.Input(InputMsg{
		Source:      "operator:editable-cockpit",
		ClientMsgID: strconv.FormatUint(editSeq, 10),
		Control: func(scene *Scene) {
			for _, patch := range batch {
				scene.applyInput(InputMsg{
					Path:   patch.Path,
					Value:  patch.Value,
					Source: "operator:editable-cockpit",
				})
			}
		},
	}) {
		return ErrPreviewBusy
	}
	cur.editSeq = editSeq
	return nil
}

// ApplyPreviewInput queues a service-originated input on the currently active
// editable Preview clone. This is deliberately separate from the Program
// adapter path: a local Quasar component event may update the Preview clone
// (and any explicit editable-air mirror) without being routed through the
// antenne Show or mutating a Blue/Program scene. The input is not an authoring
// edit, so it does not advance ZabCanvas's edit sequence.
func (p *PreviewSlot) ApplyPreviewInput(path string, value json.RawMessage, source string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.current
	if cur == nil {
		return ErrPreviewSceneMismatch
	}
	if !cur.editable {
		return ErrPreviewNotEditable
	}
	if path == "" || strings.HasPrefix(path, "__test.") {
		return ErrPreviewEditPath
	}
	if _, ok := cur.scene.DeclaredKeyspace()[path]; !ok {
		return ErrPreviewEditPath
	}
	if len(value) == 0 || !json.Valid(value) {
		return ErrPreviewEditPath
	}
	if source == "" {
		source = "service:preview-input"
	}
	if !cur.scene.Input(InputMsg{
		Path:   path,
		Value:  append(json.RawMessage(nil), value...),
		Source: source,
	}) {
		return ErrPreviewBusy
	}
	return nil
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
	stopped := make(map[*previewClone]struct{}, len(p.editable)+1)
	if p.current != nil {
		p.current.scene.Stop()
		p.wire.Drop(p.current.sceneID)
		stopped[p.current] = struct{}{}
	}
	for sceneID, clone := range p.editable {
		if _, ok := stopped[clone]; !ok {
			clone.scene.Stop()
			p.wire.Drop(sceneID)
		}
	}
	p.current = nil
	clear(p.editable)
}
