package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// Probe tests for the API-layer scene-validation helpers (ADR 003 §3.2.2,
// issue #87). These exercise computeCampaign (the pure, no-store,
// no-goroutine inner helper) covering the B10 guard short-circuit and
// malformed exec-program fail-loud paths. Refs #87.

func probeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func probeHarnessDeps() PublicDeps {
	reg := runtime.NewComputeRegistry()
	logger := probeLogger()
	return PublicDeps{
		Logger:  logger,
		Harness: runtime.NewHarness(reg, logger, runtime.ValidationBudget{MaxSteps: 50_000, MaxWall: 0}),
	}
}

// TestComputeCampaign_PureSceneValidates: a graph with no exec programs
// (the current prod state, R9) returns StatusValidated immediately.
func TestComputeCampaign_PureSceneValidates(t *testing.T) {
	deps := probeHarnessDeps()
	graph := &compiler.Graph{SceneID: "pure", SceneVersion: "sha256:pure"}
	bundle := &compiler.RenderBundle{}

	_, status := computeCampaign(deps, graph, bundle)
	if status != runtime.StatusValidated {
		t.Fatalf("pure scene status = %s, want validated", status)
	}
}

// TestComputeCampaign_MalformedExecProgramFails: a graph whose exec_programs
// array contains unparseable JSON fails loudly with StatusFailed — an
// unproven (undecodable) scene never reaches air (fail-loud, ADR §2).
func TestComputeCampaign_MalformedExecProgramFails(t *testing.T) {
	deps := probeHarnessDeps()
	graph := &compiler.Graph{
		SceneID:      "bad-prog",
		SceneVersion: "sha256:bad",
		ExecPrograms: []json.RawMessage{json.RawMessage(`{bad json`)},
	}
	bundle := &compiler.RenderBundle{}

	_, status := computeCampaign(deps, graph, bundle)
	if status != runtime.StatusFailed {
		t.Fatalf("malformed exec program status = %s, want failed", status)
	}
}

// TestComputeCampaign_TerminatingProgramValidates: a well-formed exec
// program whose on-start entrypoint terminates within budget produces
// StatusValidated.
func TestComputeCampaign_TerminatingProgramValidates(t *testing.T) {
	deps := probeHarnessDeps()
	prog := &runtime.ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*runtime.ExecNode{
			"set": {ID: "set", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{
					"name":  json.RawMessage(`"x"`),
					"value": json.RawMessage(`1`),
				}},
		},
		Entrypoints: map[string]runtime.ExecEntry{
			"start": {Kind: runtime.EntryOnStart, Target: runtime.ExecTarget{Node: "set"}},
		},
	}
	rawProg, err := json.Marshal(prog)
	if err != nil {
		t.Fatal(err)
	}
	graph := &compiler.Graph{
		SceneID:      "ok-prog",
		SceneVersion: "sha256:ok",
		ExecPrograms: []json.RawMessage{rawProg},
		Defaults:     map[string]json.RawMessage{},
	}
	bundle := &compiler.RenderBundle{}

	_, status := computeCampaign(deps, graph, bundle)
	if status != runtime.StatusValidated {
		t.Fatalf("terminating program status = %s, want validated", status)
	}
}

// TestComputeCampaign_DivergentProgramFails: a divergent while-loop
// crosses the step budget and produces StatusFailed. The harness fails
// the scene, the campaign persists StatusFailed — this version never
// reaches air (doctrine: budget bounds the PROOF, never kills).
func TestComputeCampaign_DivergentProgramFails(t *testing.T) {
	deps := probeHarnessDeps()
	prog := &runtime.ExecProgram{
		BlueprintKey: "bp",
		Nodes: map[string]*runtime.ExecNode{
			"while": {ID: "while", Op: runtime.OpWhile,
				Data: []runtime.ExecDataInput{{Port: "condition", From: "lit.true"}},
				Next: map[string]runtime.ExecTarget{"body": {Node: "noop"}}},
			"noop": {ID: "noop", Op: runtime.OpVariableSet,
				Config: map[string]json.RawMessage{
					"name":  json.RawMessage(`"tick"`),
					"value": json.RawMessage(`1`),
				}},
		},
		Entrypoints: map[string]runtime.ExecEntry{
			"start": {Kind: runtime.EntryOnStart, Target: runtime.ExecTarget{Node: "while"}},
		},
	}
	rawProg, err := json.Marshal(prog)
	if err != nil {
		t.Fatal(err)
	}
	graph := &compiler.Graph{
		SceneID:      "bad-while",
		SceneVersion: "sha256:while",
		ExecPrograms: []json.RawMessage{rawProg},
		Defaults: map[string]json.RawMessage{
			"lit.true": json.RawMessage(`true`),
		},
		Nodes: []compiler.GraphNode{{ID: "lit.true", Kind: "input", Path: "lit.true"}},
	}
	bundle := &compiler.RenderBundle{}

	_, status := computeCampaign(deps, graph, bundle)
	if status != runtime.StatusFailed {
		t.Fatalf("divergent while status = %s, want failed", status)
	}
}
