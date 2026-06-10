package runtime

import (
	"encoding/json"
	"strconv"
	"time"
)

// The exec interpreter (ADR 003 §3.1, issue #82). A task is an
// EXPLICIT, RESUMABLE continuation — a frame stack plus a value
// environment — never a blocked Go call stack and never a goroutine
// (goroutine-per-task was explicitly rejected, §3.1.7: racy ordering,
// non-determinism, untestable at 20 k). One stepTask call advances the
// task by exactly one bounded step; the scene loop interleaves steps
// with inbox drains and dataflow recomputes, so a `while(true)` runs
// faithfully forever while the loop stays reactive.

// execFrameKind discriminates continuation frames.
type execFrameKind uint8

const (
	// frameNode: execute one exec node (entered through `port`), then
	// continue with whatever it pushes.
	frameNode execFrameKind = iota
	// frameSeq: a sequence in progress — fires then_<idx> next.
	frameSeq
	// frameLoop: a loop in progress — next iteration or `completed`.
	frameLoop
)

// execFrame is one continuation frame. The mutable fields (idx) are
// task-local: programs are shared read-only, frames are not.
type execFrame struct {
	kind execFrameKind
	node string // frameNode: node to execute; frameSeq/frameLoop: owner
	port string // frameNode: exec in-pin the edge lands on
	idx  int    // frameSeq: next sibling; frameLoop: next index
	last int    // for-loop: inclusive end
	items []json.RawMessage // for-each: snapshot of the iterated list
}

// execTask is one live exec task: an id (deterministic, per-scene
// monotonic), the continuation stack and the value environment that
// carries per-iteration pins (`<nodeID>.index`, `<nodeID>.element`).
// All of it is plain data owned by the scene goroutine — re-enqueueing
// or parking a task moves a pointer, never suspends a goroutine.
type execTask struct {
	id     uint64
	frames []execFrame
	env    map[string]json.RawMessage
	steps  uint64
}

func (t *execTask) pushNode(tgt ExecTarget) {
	t.frames = append(t.frames, execFrame{kind: frameNode, node: tgt.Node, port: tgt.Port})
}

// execOpOutcome is what an extension op (registerExecOp) reports back.
type execOpOutcome struct {
	// park forks a continuation: the chain resumes at `resume` when
	// `parkKey` is woken (resumeParked / InputMsg.ResumeExec). The
	// surrounding task continues with its remaining frames — UE
	// latent semantics: a sequence (or loop) proceeds with its next
	// sibling while the suspended child waits (ADR 003 §3.1.3).
	park    bool
	parkKey string
	resume  ExecTarget
	// timer + deadline arm a timer-wheel entry for the parked
	// continuation (issue #83's `delay`): at the deadline, the wheel
	// resumes parkKey on the scene goroutine. Only honoured when the
	// park is accepted (B8 cap / duplicate key shed nothing onto the
	// wheel).
	timer    bool
	deadline time.Time
	// start, when non-nil, runs ONLY after the park was accepted
	// (B8 cap / duplicate key shed nothing into the world) — the
	// phase-3 async effects submit their worker-pool job through it,
	// so a shed park never leaves an orphan in-flight effect.
	start func()
	// next, when non-nil and !park, overrides the default
	// `then` continuation.
	next *ExecTarget
	// halt, when true and !park, ends THIS chain here (no default
	// `then`): the effect completion path uses it when the relevant
	// out pin is unwired. The task's other frames keep running.
	halt bool
}

// execOpFn is an extension exec op. Runs on the scene goroutine.
// Issue #83 registers `delay` here (park on the timer wheel); phase 3
// registers the async effects.
type execOpFn func(s *Scene, t *execTask, node *ExecNode, inPort string) execOpOutcome

// stepTask advances the task by exactly one interpreter step: one node
// execution or one control-frame advance. Bounded by construction —
// the time-slice accounting in runExecSlice counts calls to this.
func (s *Scene) stepTask(t *execTask) {
	t.steps++
	top := len(t.frames) - 1
	f := t.frames[top]
	switch f.kind {
	case frameNode:
		t.frames = t.frames[:top]
		s.execNode(t, f.node, f.port)

	case frameSeq:
		node := s.execProg.Nodes[f.node]
		if node == nil || f.idx >= len(node.seqTargets) {
			t.frames = t.frames[:top]
			return
		}
		tgt := node.seqTargets[f.idx]
		t.frames[top].idx++
		t.pushNode(tgt)

	case frameLoop:
		node := s.execProg.Nodes[f.node]
		if node == nil {
			t.frames = t.frames[:top]
			return
		}
		switch node.Op {
		case OpForLoop:
			if f.idx <= f.last {
				t.setEnvInt(node.ID, "index", f.idx)
				t.frames[top].idx++
				s.pushLoopBody(t, node)
			} else {
				t.frames = t.frames[:top]
				t.pushNodeIfNext(node, "completed")
			}
		case OpForEach:
			if f.idx < len(f.items) {
				t.env[node.ID+".element"] = f.items[f.idx]
				t.setEnvInt(node.ID, "index", f.idx)
				t.frames[top].idx++
				s.pushLoopBody(t, node)
			} else {
				t.frames = t.frames[:top]
				t.pushNodeIfNext(node, "completed")
			}
		case OpWhile:
			// The condition is re-pulled EVERY iteration through the
			// data layer (demand evaluation over current state), so a
			// body effect (variable.set) is observed by the next
			// check. No iteration cap — doctrine §1.1.
			if s.pullBool(t, node, "condition") {
				s.pushLoopBody(t, node)
			} else {
				t.frames = t.frames[:top]
				t.pushNodeIfNext(node, "completed")
			}
		default:
			s.logger.Error("exec: loop frame on non-loop node", "node", f.node, "op", node.Op)
			t.frames = t.frames[:top]
		}
	}
}

// pushLoopBody pushes the loop body chain (if wired — a body-less loop
// still iterates and fires `completed`).
func (s *Scene) pushLoopBody(t *execTask, node *ExecNode) {
	if tgt, ok := node.next("body", "loop_body"); ok {
		t.pushNode(tgt)
	}
}

func (t *execTask) pushNodeIfNext(node *ExecNode, pin string) {
	if tgt, ok := node.next(pin); ok {
		t.pushNode(tgt)
	}
}

// execNode executes one exec node entered through inPort. A defective
// reference (unknown node / unregistered op) ends THAT CHAIN loudly
// (error log) — the task's remaining frames (sequence siblings, loop
// iterations) keep running; nothing is killed. The conformance matrix
// (criterion 1) is what guarantees this branch never fires for a
// manifest-known node type in the final state.
func (s *Scene) execNode(t *execTask, id, port string) {
	node := s.execProg.Nodes[id]
	if node == nil {
		s.logger.Error("exec: unknown node id", "node", id)
		return
	}
	// Extension ops first (delay/#83, async effects/phase 3, tests).
	if fn, ok := s.execOps[node.Op]; ok {
		out := fn(s, t, node, port)
		if out.park {
			// Fork the suspended chain into its own continuation:
			// the surrounding task continues with its remaining
			// frames (sequence siblings / loop iterations) — UE
			// latent semantics. The environment is snapshotted so
			// per-iteration pins stay correct at resume time.
			cont := &execTask{id: t.id, env: copyEnv(t.env)}
			cont.pushNode(out.resume)
			if s.parkTask(out.parkKey, cont) {
				if out.timer {
					s.wheelAdd(out.parkKey, out.deadline)
				}
				if out.start != nil {
					out.start()
				}
			}
			// A park may ALSO continue the current task immediately:
			// `animation.play`'s `then` fires now while `completed`
			// waits parked (issue #86). Honoured even when the park was
			// shed (B8/dup) — only the completion continuation is lost,
			// counted; the immediate path is never amputated (§1.1).
			if out.next != nil {
				t.pushNode(*out.next)
			}
			return
		}
		if out.halt {
			return
		}
		if out.next != nil {
			t.pushNode(*out.next)
			return
		}
		t.pushNodeIfNext(node, "then")
		return
	}

	switch node.Op {
	case OpBranch:
		if s.pullBool(t, node, "condition") {
			t.pushNodeIfNext(node, "true")
		} else {
			t.pushNodeIfNext(node, "false")
		}

	case OpSequence:
		t.frames = append(t.frames, execFrame{kind: frameSeq, node: id})

	case OpForLoop:
		first := s.pullInt(t, node, "first", 0)
		last := s.pullInt(t, node, "last", -1)
		t.frames = append(t.frames, execFrame{kind: frameLoop, node: id, idx: first, last: last})

	case OpForEach:
		items := s.pullArray(t, node, "list")
		t.frames = append(t.frames, execFrame{kind: frameLoop, node: id, items: items})

	case OpWhile:
		t.frames = append(t.frames, execFrame{kind: frameLoop, node: id})

	case OpGate:
		s.execGate(t, node, port)

	case OpVariableSet:
		s.execVariableSet(t, node)
		t.pushNodeIfNext(node, "then")

	case OpPrint:
		s.execPrint(t, node)
		t.pushNodeIfNext(node, "then")

	default:
		s.logger.Error("exec: unregistered exec op", "op", node.Op, "node", id)
	}
}

// execGate implements UE gate semantics: persistent open/closed state
// under `__nodestate.<node_id>` (ADR 003 §3.1.3), reseeded from
// `start_closed` on restart (the leaf is not in graph.Defaults, so a
// cold start falls back to the config — criterion 11 holds). `enter`
// passes through to `exit` iff open; `open`/`close`/`toggle` mutate
// the state and fire nothing.
func (s *Scene) execGate(t *execTask, node *ExecNode, port string) {
	leaf := "__nodestate." + node.ID
	open := !configBool(node.Config, "start_closed")
	if raw, ok := s.state.Get(leaf); ok {
		var st struct {
			Open bool `json:"open"`
		}
		if err := json.Unmarshal(raw, &st); err == nil {
			open = st.Open
		}
	}
	setOpen := func(v bool) {
		if v {
			s.effector.SetLeaf(leaf, json.RawMessage(`{"open":true}`))
		} else {
			s.effector.SetLeaf(leaf, json.RawMessage(`{"open":false}`))
		}
	}
	switch port {
	case "open":
		setOpen(true)
	case "close":
		setOpen(false)
	case "toggle":
		setOpen(!open)
	default: // "enter" (or an unspecified pin on a pre-partition artefact)
		if open {
			t.pushNodeIfNext(node, "exit")
		}
	}
}

// execVariableSet writes `__vars.<blueprint_key>.<name>` through the
// effect interface — intra-goroutine, into THIS scene instance's state
// only (B9): two instances sharing a blueprint_key have fully disjoint
// `__vars`, because the path is the same but the State object is not,
// and nothing ever routes the write through the cross-scene inbox
// fan-out.
func (s *Scene) execVariableSet(t *execTask, node *ExecNode) {
	name := configString(node.Config, "name")
	if name == "" {
		s.logger.Error("exec: variable.set without name", "node", node.ID)
		return
	}
	val, ok := s.pullData(t, node, "value")
	if !ok {
		val = json.RawMessage(`null`)
	}
	s.effector.SetLeaf("__vars."+s.execProg.BlueprintKey+"."+name, val)
}

func (s *Scene) execPrint(t *execTask, node *ExecNode) {
	var line string
	if raw, ok := s.pullData(t, node, "message"); ok {
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			line = str
		} else {
			line = string(raw)
		}
	}
	s.effector.Print(s.execProg.BlueprintKey, line)
}

// --- demand-driven data pulls (ADR 003 §3.1.1) ------------------------

// pullData resolves one data input of an exec node: the wired producer
// evaluated on demand, else the node's config value under the same
// name (Blue's unwired-port default convention), else absent.
func (s *Scene) pullData(t *execTask, node *ExecNode, port string) (json.RawMessage, bool) {
	for _, di := range node.Data {
		if di.Port != port {
			continue
		}
		memo := map[string]json.RawMessage{}
		v := s.demandValue(t, di.From, di.FromPort, memo)
		if v == nil {
			return nil, false
		}
		return v, true
	}
	if v, ok := node.Config[port]; ok {
		return v, true
	}
	return nil, false
}

// demandValue evaluates the pure upstream cone of a data pull — same
// compute functions, same registry as the dataflow layer, but pulled
// on demand so per-iteration env pins (loop `index`/`element`) are
// observed with their CURRENT value mid-task, not the value the last
// push-recompute saw. Computed nodes are re-evaluated (memoized per
// pull — the cone is a DAG, so this terminates); input-kind nodes and
// unknown ids read state. Reads only — single-writer untouched.
func (s *Scene) demandValue(t *execTask, from, fromPort string, memo map[string]json.RawMessage) json.RawMessage {
	// Exec-node data out (loop pins), bound in the task environment.
	if fromPort != "" {
		if v, ok := t.env[from+"."+fromPort]; ok {
			return v
		}
	}
	if v, ok := memo[from]; ok {
		return v
	}
	idx, ok := s.nodeIdx[from]
	if !ok {
		// Not a dataflow node: a raw state leaf (or an exec node id —
		// nothing bound, nothing to read).
		if v, ok := s.state.Get(s.upstreamPath(from)); ok {
			return v
		}
		return nil
	}
	ce := s.computeOrder[idx]
	if ce.node.Kind == "input" {
		if v, ok := s.state.Get(s.upstreamPath(from)); ok {
			return v
		}
		return nil
	}
	// Pure compute: gather inputs recursively under the same named /
	// positional convention as gatherInputs, then run the same fn.
	args := make(map[string]json.RawMessage, len(ce.upstream))
	if ins := ce.node.Inputs; len(ins) > 0 {
		for i, in := range ins {
			name := in.Port
			if name == "" {
				name = positionalPort(i)
			}
			if v := s.demandValue(t, in.From, "", memo); v != nil {
				args[name] = v
			}
		}
	} else {
		for i, up := range ce.upstream {
			if v := s.demandValue(t, up, "", memo); v != nil {
				args[positionalPort(i)] = v
			}
		}
	}
	fn, err := s.cmpReg.Get(ce.node.Compute)
	if err != nil {
		s.logger.Error("exec pull: unknown compute", "compute", ce.node.Compute, "node", ce.node.ID, "err", err)
		return nil
	}
	val, err := fn(args, ce.node.Config)
	if err != nil {
		s.logger.Warn("exec pull: compute error", "compute", ce.node.Compute, "node", ce.node.ID, "err", err)
		return nil
	}
	memo[from] = val
	return val
}

func positionalPort(i int) string {
	names := [...]string{"a", "b", "c", "d"}
	return names[i%len(names)]
}

func (s *Scene) pullBool(t *execTask, node *ExecNode, port string) bool {
	raw, ok := s.pullData(t, node, port)
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		s.logger.Warn("exec: data input not a boolean", "node", node.ID, "port", port, "value", string(raw))
		return false
	}
	return b
}

// pullInt reads an integer data input. JSON numbers arrive as float64;
// int(f) truncates toward zero and maps IEEE-754 -0 to int 0, so a
// `-0` bound can never flip a loop-direction or sign comparison.
func (s *Scene) pullInt(t *execTask, node *ExecNode, port string, def int) int {
	raw, ok := s.pullData(t, node, port)
	if !ok {
		return def
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		s.logger.Warn("exec: data input not a number", "node", node.ID, "port", port, "value", string(raw))
		return def
	}
	return int(f)
}

func (s *Scene) pullArray(t *execTask, node *ExecNode, port string) []json.RawMessage {
	raw, ok := s.pullData(t, node, port)
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		s.logger.Warn("exec: data input not an array", "node", node.ID, "port", port, "value", string(raw))
		return nil
	}
	return items
}

func (t *execTask) setEnvInt(nodeID, port string, v int) {
	t.env[nodeID+"."+port] = json.RawMessage(strconv.Itoa(v))
}

func copyEnv(env map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

func configString(cfg map[string]json.RawMessage, key string) string {
	raw, ok := cfg[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func configBool(cfg map[string]json.RawMessage, key string) bool {
	raw, ok := cfg[key]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false
	}
	return b
}
