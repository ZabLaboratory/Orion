package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type SceneSelection struct {
	SceneID  string `json:"scene_id"`
	Version  string `json:"scene_version"`
	StreamID string `json:"stream_id"`
}
type SceneControlDocument struct {
	LSML     string         `json:"lsml"`
	Schema   string         `json:"schema"`
	SceneID  string         `json:"scene_id"`
	Layout   map[string]any `json:"layout"`
	Defaults struct {
		Desired  map[string]SceneSelection `json:"desired"`
		Observed map[string]any            `json:"observed"`
	} `json:"defaults"`
}

func NewSceneControlDocument() SceneControlDocument {
	d := SceneControlDocument{LSML: "1.2", Schema: "orion.scene-control.v1", SceneID: "orion-scene-control", Layout: map[string]any{"type": "frame", "children": []any{}}}
	d.Defaults.Desired = map[string]SceneSelection{}
	d.Defaults.Observed = map[string]any{}
	return d
}

type SceneControl struct {
	deps      SceneIntentDeps
	reception *lsdpreception.Reception
	path      string
	headers   http.Header
	mu        sync.Mutex
	desired   map[string]SceneSelection
}

func OpenSceneControl(deps SceneIntentDeps, reception *lsdpreception.Reception, path string, headers http.Header) (*SceneControl, error) {
	if deps.Catalog == nil || !deps.EmbeddedLocal || !filepath.IsAbs(path) {
		return nil, errors.New("SCENE_CONTROL_CONFIG_INVALID")
	}
	c := &SceneControl{deps: deps, reception: reception, path: path, headers: headers.Clone(), desired: map[string]SceneSelection{}}
	raw, err := os.ReadFile(path)
	if err == nil {
		if len(raw) > 65536 {
			return nil, errors.New("SCENE_CONTROL_LIMIT")
		}
		var d SceneControlDocument
		if json.Unmarshal(raw, &d) != nil || validateSceneControl(d) != nil {
			return nil, errors.New("SCENE_CONTROL_INVALID")
		}
		c.desired = d.Defaults.Desired
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return c, nil
}
func validateSceneControl(d SceneControlDocument) error {
	if d.LSML != "1.2" || d.Schema != "orion.scene-control.v1" || d.SceneID != "orion-scene-control" || d.Layout == nil || d.Defaults.Desired == nil || d.Defaults.Observed == nil || len(d.Defaults.Desired) > 2 {
		return errors.New("SCENE_CONTROL_INVALID")
	}
	for lane, s := range d.Defaults.Desired {
		if lane != "program" && lane != "preview" || s.SceneID == "" || s.Version == "" || s.StreamID == "" || len(s.SceneID) > 128 || len(s.Version) > 128 || len(s.StreamID) > 128 {
			return errors.New("SCENE_CONTROL_SELECTION_INVALID")
		}
	}
	return nil
}
func selectionKey(s SceneSelection) string {
	raw, _ := json.Marshal(s)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// Run watches only the shared native resource. Changes to observed fields never
// trigger another Blue activation. Each lane cancels/joins its own predecessor.
func (c *SceneControl) Run(ctx context.Context) error {
	state, err := c.reception.Read(ctx, "orion/state")
	if err != nil {
		return err
	}
	root, _ := state.(map[string]any)
	if root["scene_control"] == nil {
		d := NewSceneControlDocument()
		d.Defaults.Desired = c.desired
		if err = c.reception.WriteLeaf(ctx, "orion/state", "/scene_control", d); err != nil {
			return err
		}
	}
	queues := map[string]chan SceneSelection{"program": make(chan SceneSelection, 1), "preview": make(chan SceneSelection, 1)}
	var workers sync.WaitGroup
	for lane, queue := range queues {
		workers.Add(1)
		go func(lane string, queue chan SceneSelection) { defer workers.Done(); c.lane(ctx, lane, queue) }(lane, queue)
	}
	defer workers.Wait()
	c.reception.Watch(ctx, "orion/state", func(state any) {
		root, _ := state.(map[string]any)
		raw, err := json.Marshal(root["scene_control"])
		var d SceneControlDocument
		if err != nil || len(raw) > 65536 || json.Unmarshal(raw, &d) != nil || validateSceneControl(d) != nil {
			c.log("invalid native scene control", errors.New("SCENE_CONTROL_INVALID"))
			return
		}
		c.mu.Lock()
		c.desired = d.Defaults.Desired
		c.mu.Unlock()
		for lane, selection := range d.Defaults.Desired {
			queue := queues[lane]
			select {
			case queue <- selection:
			default:
				select {
				case <-queue:
				default:
				}
				queue <- selection
			}
		}
	}, func(err error) { c.log("scene-control subscription reconnecting", err) })
	return ctx.Err()
}
func (c *SceneControl) lane(ctx context.Context, lane string, queue chan SceneSelection) {
	var current string
	var cancel context.CancelFunc
	var done chan struct{}
	defer func() {
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case selection := <-queue:
			key := selectionKey(selection)
			if key == current {
				continue
			}
			if cancel != nil {
				cancel()
			}
			if done != nil {
				<-done
			}
			current = key
			attempt, stopAttempt := context.WithTimeout(ctx, 40*time.Second)
			cancel = stopAttempt
			taskDone := make(chan struct{})
			done = taskDone
			go func() { defer close(taskDone); defer stopAttempt(); c.selectScene(attempt, lane, selection) }()
		}
	}
}
func (c *SceneControl) persist() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := NewSceneControlDocument()
	d.Defaults.Desired = c.desired
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return writeControlFile(c.path, raw)
}
func (c *SceneControl) selectScene(ctx context.Context, lane string, selection SceneSelection) {
	id, err := native.NewID()
	if err != nil {
		c.log("selection identity unavailable", err)
		return
	}
	observe := func(status, reason string) {
		ackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		value := map[string]any{"activation_id": id, "scene_id": selection.SceneID, "scene_version": selection.Version, "status": status}
		if reason != "" {
			value["reason"] = reason
		}
		if err := c.reception.WriteLeaf(ackCtx, "orion/state", "/scene_control/defaults/observed/"+lane, value); err != nil {
			c.log("selection observation unavailable", err)
		}
	}
	if err = c.persist(); err != nil {
		observe("failed", "SCENE_CONTROL_PERSIST_FAILED")
		return
	}
	observe("preparing", "")
	action := attestation.ActionPreparePreview
	if lane == "program" {
		action = attestation.ActionTakeOnAir
	}
	principal := authSource.FromHeaders(c.headers).UserID
	req, err := c.deps.Catalog.get(principal, selection.SceneID, selection.Version, selection.StreamID, string(action))
	if err != nil {
		observe("failed", "SCENE_CATALOG_UNAVAILABLE")
		return
	}
	req.IntentID = id
	req.IdempotencyKey = id
	raw, err := json.Marshal(req)
	if err != nil {
		observe("failed", "SCENE_CATALOG_INVALID")
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/api/v1/host/scene-intent", bytes.NewReader(raw))
	if err != nil {
		observe("failed", "SCENE_CONTROL_REQUEST_FAILED")
		return
	}
	request.Header = c.headers.Clone()
	response := &controlResponse{header: http.Header{}}
	postSceneIntent(c.deps)(response, request)
	var result sceneIntentResponse
	if json.Unmarshal(response.body.Bytes(), &result) != nil {
		observe("failed", "SCENE_CONTROL_RESPONSE_INVALID")
		return
	}
	if response.status != http.StatusOK {
		status := "failed"
		if ctx.Err() != nil {
			status = "superseded"
		}
		observe(status, result.Reason)
		return
	}
	observe("active", "")
}
func (c *SceneControl) log(message string, err error) {
	if c.deps.Logger != nil {
		c.deps.Logger.Warn(message, "err", err)
	}
}

type controlResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *controlResponse) Header() http.Header { return w.header }
func (w *controlResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *controlResponse) Write(bytes []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.body.Len()+len(bytes) > 65536 {
		return 0, fmt.Errorf("scene control response limit")
	}
	return w.body.Write(bytes)
}
