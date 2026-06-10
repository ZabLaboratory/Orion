# ADR 006 — Exec activation : compiler partition + R9 lift (exec to air behind the validation gate)

- **Status**: proposed
- **Date**: 2026-06-10
- **Decided**: —
- **Deciders**: @ClodoCapeo
- **Author**: Atlas (architect agent)
- **Supersedes**: — (executes and completes ADR 003 §3.1.2/§3.4 ; supersedes
  ADR 003 resolution criterion #20's *pre-phase-4 negative test* and the v2
  scaffold's criterion 18 — see §3.2)
- **Superseded by**: —

---

> **Numbering.** Per-repo convention (ADR 001/003 header notes): committed
> files in this repo are 001–003. The literal next number, 004, is burned by
> 57 in-tree citations of the uncommitted design reference « ADR 004 — Orion
> v2 runtime » (e.g. `compile.go` « per ADR 004 §7.2 ») ; 005 (Quasar) and
> 007 (LSDP, 66 citations in `show.go`/LSML code) are likewise burned. 006 is
> referenced nowhere in this tree (Blue's own ADR 006 lives in Blue's per-repo
> space, which the convention already tolerates). First unambiguous number:
> **006**.

## 1. Context

ADR 003 (accepted) shipped phases 0–5 end to end: the exec interpreter with
continuations and time-slicing (#82), timer wheel and triggers (#83), async
effects (http/db/source/animation, phase 3), the scene-validation gate (#87)
and the CI conformance matrix (#88). **All of it is dormant on air.** Verified
in code, 2026-06-10:

- The compiler emits **no `ExecProgram`**: `graph.ExecPrograms`
  (`compiler/graph.go:46`, the seam posed by #87, `omitempty`) is empty for
  every prod artefact; `runtime.ExecProgramsFromGraph` returns nil.
- `validateBlueprint` (`compile.go:539`) still **rejects every impure node**
  with `IMPURE_COMPUTE` — `delay`, `variable.set`, `print`, `http.request`,
  `db.query`, `source.read`, `animation.play` cannot reach the runtime. This
  rejection is a **vestige of the reversed dataflow-only ADR**: doctrine
  (ADR 003 §1.1, maintainer) says Orion serves the entirety of Blue and
  *rejecting a node at compile is not an option*.
- Worse, the **pure** exec-flow nodes (`branch`, `sequence`, `gate`, loops —
  `is_pure: true` in Blue's `node_purity.py`) **pass** today's compile and
  land in the data graph as `computed` nodes with no registered compute → the
  exact silent-skip residue ADR 003 §1.2 condemned. The partition kills it.
- `Show.Load → LoadExec(…, nil)` on every prod path (push, boot, rollback);
  exec is exercised by tests and the validation harness only (R9 dormancy,
  Amendment 1 §3 / condition C2).
- The **gate #87 is live and fail-closed on all three activation/mutation
  paths** (`api/gate.go`; `postActiveScene`; push-swap of an active scene —
  antenna keeps the last validated version; rollback), keyed by
  `scene_validations(scene_id, scene_version, harness_version)`.

This is the **R9 lock** of ADR 003 §3.4: no exec-bearing scene reaches air
until the gate is live. The gate IS live. The phase-ordering contract is
satisfied; the lift is now possible — and it is the last step that makes the
maintainer's doctrine real on the antenna.

## 2. Decision drivers

1. **Doctrine §1.1 (maintainer, non-negotiable)**: full Blue on air; no
   capability rejection; safety is the validation gate, never an engine or
   compile limitation.
2. **B3 invariant (Bastion, critical)**: only a *validated* version may touch
   the antenna — the lift must not open any path around #87.
3. **One interpreter, one semantics**: the program the harness proves must be
   byte-identically the program that airs (no validation/live drift, R6).
4. **Deterministic artefacts**: `scene_version` hashing must stay stable;
   pure-dataflow scenes must produce byte-identical artefacts (non-regression
   for everything currently on air).
5. **Minimal new surface**: the lift wires existing, proven pieces; it adds
   no new engine capability.

## 3. Decision

**Verdict: GO — direct enablement, gated by #87, as one coherent chantier.**
No descope: every piece below is required for the first exec scene to air
safely, and nothing beyond them is.

### 3.1 Compiler partition — emitting `ExecPrograms`

**Discriminator: exec-pin presence, not purity.** A blueprint node belongs to
the **exec layer** iff it carries at least one port with `kind == "exec"`
(`BlueprintPort.Kind`, seeded by Blue on every port — `types.go:246`,
stdlib_seeder). `is_pure` is **scheduling metadata only** (ADR 003 §3.1.2):
it marks data-layer nodes memoizable; it routes nothing. (Purity cannot
partition: `branch` is pure yet exec; `variable.get` is pure and data.)

Per blueprint (each `BlueprintRef` of the normalised list), the compiler
builds **one `ExecProgram`** (`BlueprintKey` = the ref key):

| Blueprint node | Routing |
|---|---|
| `core.event.{on-start,on-tick,on-event}@1` | `ExecEntry` with `Kind` mapped to `EntryOnStart/OnTick/OnEvent`; `Event` from `config` for on-event; `Node` = node id (data-out namespace) |
| exec-pin nodes (`core.flow.*`, `core.variable.set`, `core.print`, `core.http.request`, `core.db.query`, `core.source.read`, `core.animation.play`, `delay`) | `ExecNode` with `Op` mapped from the manifest id onto the runtime vocabulary (`OpBranch`, `OpSequence`, `OpGate`, `OpForLoop`, `OpForEach`, `OpWhile`, `OpDelay`, `OpVariableSet`, `OpPrint`, and the effect ops of `exec_effects.go` / `exec_anim.go`); `Config` carried verbatim |
| exec edge (both ports `kind=="exec"`) | `Next[<from_port>] = ExecTarget{Node, Port}` (Blue validates exec out-pins single-wired) |
| data edge **into** an exec node | `ExecDataInput{Port, From, FromPort}` — the value is pulled on demand through the data layer (ADR 003 §3.1.1) |
| pure data nodes (incl. `variable.get`, `quasar.*` leaves, `core.output/input/literal`) | data layer, **unchanged** (today's `GraphNode` path) |

**Exec nodes are removed from the data `GraphNode` list.** That closes the
silent-skip hole for pure flow nodes (§1) — an exec node is interpreted,
never "recomputed".

Emission is **deterministic**: programs serialized in blueprint-key order
into `graph.ExecPrograms` (one raw-JSON entry per blueprint, matching what
`ExecProgramsFromGraph` and the harness already consume). A blueprint with
no exec nodes emits **no program**; a scene with no exec nodes carries an
empty `ExecPrograms` → **byte-identical artefact** to today (`omitempty`),
so every existing `scene_version` hash is untouched.

Remaining compile diagnostics are **structural only** (never capability):
unknown manifest definition (existing); exec edge landing on a data pin or
vice-versa (mirror of Blue's own validation, defense-in-depth); a
manifest-known exec node with no runtime op mapping → fail-loud
`EXEC_OP_UNMAPPED` — unreachable on a conformant build (ADR 003 criterion 1
guarantees executor coverage), present so a gap can never become
accept-then-ignore.

### 3.2 Death of `IMPURE_COMPUTE`

The `!entry.IsPure → reject` branch of `validateBlueprint` (`compile.go:539`)
is **deleted**, along with `ErrImpureCompute` (`diagnostics.go:28`) and
`protocol.CodeImpureCompute` (`messages.go:59` — verified unreferenced in
Prism/Blue/Solar). **The replacement guard is the gate #87, already live**:
capability is accepted at compile; *proof* is required before air
(`SCENE_NOT_VALIDATED`, fail-closed, on activation, push-swap and rollback).
Nothing replaces the rejection *at compile* — that is the decision: the
compile stage validates structure, the gate validates behaviour.

Supersessions this entails (tracked, not silent):

- **v2 scaffold criterion 18** (« Impure compute reject », `CLAUDE.md`
  coverage table) is **retired** — already announced by ADR 003 §3.1.2 and
  criterion #2; the `CLAUDE.md` row and the two tests
  (`compile_test.go:180`, `compile_multiblueprint_test.go:279`) are rewritten
  to assert the partition (impure node → routed to exec program, scene
  compiles). `CLAUDE.md` resync is a Scribe task.
- **ADR 003 criterion #20** (pre-phase-4 negative test: live paths refuse
  exec-bearing versions) is **superseded by its own success condition**: the
  §3.2.2 enforcement is live, so the negative test is replaced by the
  gated-acceptance test (§6 #5 below) — exec-bearing versions are accepted,
  and *unvalidated* ones are refused by `SCENE_NOT_VALIDATED` (the refusal
  moves from « exec-bearing » to « unproven », which is the doctrine).

### 3.3 Multi-program install (runtime seam correction)

`ExecProgramsFromGraph` returns **one program per blueprint**, and the
harness proves each in its own clone — but `Show.LoadExec` /
`Scene.InstallExec` accept a **single** `*ExecProgram` (test-era signature).
Decision: the install seam takes the **program set** (`[]*ExecProgram`).
One live scene hosts all of its blueprints' programs; trigger indexes merge
with entry keys namespaced `<blueprint_key>/<entry_id>` (deterministic,
sorted — preserving the no-map-iteration rule of `InstallExec`). State
isolation is untouched: `__vars.<blueprint_key>.*` namespacing (ADR 001
§3.3) and per-instance state keep B9 intact. Programs stay immutable and
shareable between instances (live + test session).

### 3.4 Activation wiring — the R9 lift itself

**Invariant (normative, B3-aligned): a scene instance in the live `Show`
roster carries exec programs iff its `scene_version` has a
`scene_validations.status = validated` record for the current
`harness_version`.** Loading is otherwise unchanged (dataflow always loads;
authoring is never blocked).

One seam — `execForAir(ctx, sceneID, version, graph)` in the API layer —
resolves the set: `isAirEligible` (existing, fail-closed) → yes:
`ExecProgramsFromGraph` (fail-loud on decode) ; no: nil. **Every** prod
call-site of `Show.Load` goes through it:

1. **Push** (`scenes_push.go`): off-air scene → load with programs iff
   eligible (a freshly-pushed version never is — new hash, no record — so in
   practice nil; the seam is there for re-push of an already-validated
   version, e.g. byte-identical content). Active scene → existing #87 logic
   unchanged: eligible → `LoadExec` with programs + `scene_changed` +
   snapshot; not eligible → antenna keeps the last validated version.
2. **Validation success** (`scenes_validate.go`): when a campaign ends
   `validated` and the validated version is still the scene's
   `latest_pushed_version`, the handler **re-loads the roster instance
   through `execForAir`** — this is both (a) how an off-air scene arms its
   programs before activation, and (b) the **deferred swap** ADR 003
   criterion #15 promises for an active scene (« once that version
   validates, the swap proceeds normally » — with `scene_changed` + fresh
   snapshot). Restart-reseed semantics make the re-load safe by design
   (defaults + `on-start` if active).
3. **Boot** (`cmd/orion/main.go::loadActiveScenes`): reseeds with programs
   iff eligible. **Normative**: omitting this would air a validated exec
   scene with its logic silently dead after a restart — a correctness bug,
   not a safety one; §6 #7 covers it.
4. **Rollback** (`handleRollback`): already gated by #87 (B-rollback);
   re-points and re-loads through `execForAir` (target is validated by
   definition → programs installed).

**Trigger firing scope (normative): exec triggers fire only while the scene
is on air, or in a test-session/validation clone.** Verified: the global
tick fans out to **every** loaded scene (`tick.go:78`) — without this rule,
an off-air, validated, loaded scene would run `on-tick` chains with **real
effects** backstage. So: `on-start` fires at activation (existing
`FireOnStart` call-site) ; `on-tick`/`on-event` are gated on the instance's
on-air flag (toggled via the inbox by `Show.SetActive`, preserving the
single-writer model) ; switch-away cancels all live tasks (ADR 003 §3.1.4,
already implemented). Backstage scenes keep full dataflow; their exec is
quiescent. Test sessions are untouched (authors iterate freely).

### 3.5 The 6 `core.db.*` inline-only atoms (conformance #88 residual)

Decision (the allowlist's open « maintainer's call → Atlas »): the atoms
`core.db.{from,where,join,select,order,limit}@1` are **reclassified
inline-only in the conformance manifest** (`gen_conformance_manifest.py`
marks them; the matrix exempts them with that reason) and **dropped from
`conformance_allowlist.txt`, which reaches its empty end-state** (ADR 003
criterion 1). Rationale: per Blue's own executor contract they are *only*
legal inside `core.db.query@1`'s `config.inline_graph` (main-graph placement
raises `db_node_outside_query` **in Blue**); they are served transitively by
the `db.query` executor (topology A). A main-graph occurrence reaching Orion
is structurally invalid input per the language's own definition → structural
diagnostic mirroring Blue's, not a capability rejection. Doctrine §1.1
intact: the language *as Blue defines it* is served in full.

### 3.6 Rollout — tranché

**Direct enablement behind #87. No feature flag, no node-subset staging.**

- A **node subset** is a capability restriction — exactly what the maintainer
  reversed; dead on arrival.
- A **feature flag** (`ORION_EXEC_LIVE`) would be a second source of truth
  beside the gate, a dormancy-regression risk, and a standing temptation to
  ship unproven paths « because the flag is off ». The gate already *is* the
  rollout control: nothing airs unvalidated, scene by scene, version by
  version — strictly finer-grained than any flag.
- **Operational first flight** (procedure, not code — runbook task): the
  first exec scene to air is a **canary scene** (maintainer-authored,
  exercising loop + variables + delay + one async effect), validated, aired
  in a controlled window with the §3.2.2 metrics watched
  (`orion_task_preempt_total`, `orion_task_budget_exceeded_total`,
  `orion_parked_tasks`, `orion_timer_wheel_size`,
  `orion_task_cpu_seconds_total`). Incident lever = the existing validated
  rollback path (ADR 003 criterion #16) — no new mechanism.
- **Escalation to the maintainer**: if an ops kill-switch is wanted despite
  the above, it is a *deployment policy* env with an explicit risk-acceptance
  note here (Amendment) — it is not part of this decision and not the
  default.

### 3.7 Blocking prerequisites (pre-lift, already tracked — inscribed as dependencies)

The **activation-wiring issue (§3.4) must not merge** before:

| # | Prerequisite | Owner / trail |
|---|---|---|
| P-1 | **B10 introspective guard**: the effect-registry enumeration must fail on any registered world-op lacking declared validation-mode inertia — introspective, not list-maintained (a world-op added later must not leak once exec is live) | Bastion condition on #87 |
| P-2 | **`matchPath` hardening** | Bastion finding on #86 |
| P-3 | **`cpus` recalibration post-nproc** (validation campaigns + worker pool sizing) | Bastion finding on #89 |
| P-4 | **ZabGate P1 header deployed in prod** — without it the `_query` and completion REST wires (`db.query` topology A, B-syswrite) are broken on air | Keeper, in flight |
| P-5 | **Bastion B1 status confirmed**: the egress-policy veto on `http.request` (ADR 003 crit. #21 ledger: « maintained ») must be confirmed lifted (default prod mode + post-DNS anti-SSRF) — or `http.request`'s live exercise is explicitly held back by *deployment policy* (not capability) until it is | Bastion |

The compiler partition (§3.1–3.3) and conformance reclass (§3.5) may merge
before these — they change no live behaviour until §3.4 lands (programs are
emitted but never installed; the invariant holds vacuously).

## 4. Consequences

- The push artefact grows `exec_programs` for exec-bearing scenes;
  pure-dataflow artefacts stay byte-identical (hashes stable, nothing on air
  re-validates).
- `IMPURE_COMPUTE` disappears from the codebase and the wire (verified
  consumer-free); v2 scaffold criterion 18 retired → `CLAUDE.md` resync
  (Scribe) ; ADR 003 criterion #20 superseded (§3.2).
- The validation gate becomes the **only** behaviour gate, on every path —
  the doctrine's end-state: capability total, proof mandatory.
- The harness, gate, conformance matrix, metrics and runbook
  (`orion-cpu-pathological-scene.md`) acquire their real workload — they
  stop guarding an empty set.
- Prism/Canvas: no contract change (diagnostics shrink by one code; the
  validation status surface from #87 is unchanged). Blue: untouched.
- ADR 003 phase table is complete; condition C2 (R9 residual: inertia by
  call-site absence) is **dissolved by a stronger invariant** (§3.4: install
  keyed on validation record, asserted by test — no longer mere absence).

## 5. Risks

| # | Risk | Posture |
|---|---|---|
| R-1 | **Validated-but-costly scene on air** (B7): validation proves termination-within-budget under fixtures, not p95 under production load (e.g. a 4.9 s on-event chain re-fired per chat message) | Observability never amputation (ADR 003 §3.2.2): metrics + alert + runbook ; canary first flight (§3.6) ; incident lever = validated rollback. Residual accepted by ADR 003 doctrine |
| R-2 | **Effect leak in validation mode** if a world-op lands without declared inertia once exec is live | **Blocked by P-1** (introspective B10 guard) — hard prerequisite, not a residual |
| R-3 | **First real exercise of B-syswrite + `_query` in prod** (external completion reports, ZabGate-delegated queries) — proven in tests/staging, never under antenna traffic | P-4 prerequisite ; §6 #8 staging soak before the canary window ; Bastion threat-model pass at implementation (gated) |
| R-4 | **Backstage exec quiescence** rests on the on-air flag (§3.4): a flaw would run real effects from off-air loaded scenes | Normative test §6 #6 ; single-writer inbox toggle keeps it race-free |
| R-5 | **Boot-path omission** airs a validated scene with dead logic after restart | §6 #7 |
| R-6 | **`http.request` egress** live while B1 unconfirmed | P-5 — explicit Bastion confirmation or documented deployment-policy hold |

Detailed threat model = **Bastion at implementation stage** (per the gated
campaign), with R-2/R-3/R-6 as the entry points.

## 6. Resolution criteria

Testable; CI-enforced where possible.

1. **Partition correctness.** A blueprint mixing pure data, pure exec-flow
   (`branch`, loop), impure exec (`variable.set`, `delay`, `http.request`)
   compiles: exec-pin nodes appear in exactly one `ExecProgram` (correct ops,
   `Next` wiring, `ExecDataInput`s), none appears as a data `GraphNode`, pure
   data nodes unchanged. Round-trips through `ExecProgramsFromGraph`.
2. **Determinism / non-regression.** Same envelope twice → byte-identical
   artefact incl. `exec_programs` order ; a pure-dataflow scene → artefact
   byte-identical to pre-lift (hash unchanged, golden test).
3. **No `IMPURE_COMPUTE`.** The code, constant and diagnostic no longer
   exist; the two rewritten compile tests assert acceptance + routing
   (ADR 003 criterion #2's final state).
4. **Multi-blueprint install.** A 2-blueprint exec scene: both programs
   installed on one instance; entries fire under namespaced keys; B9
   isolation test (live + test session) stays green.
5. **Gated acceptance E2E (replaces ADR 003 #20).** Push exec-bearing scene →
   200, version persisted ; activate → `SCENE_NOT_VALIDATED` ; `/validate` →
   `validated` ; activate → success ; **the maintainer's loop test (ADR 003
   criterion #4) passes through the real push API**: `on-start →
   for-loop(0..9) → variable.set(counter=index)` airs, subscriber observes
   `counter == 9`.
6. **Air-only triggers.** A validated exec scene loaded but **not** active:
   ticks flow, zero exec tasks fire, zero effects attempted (asserted via
   effect-seam instrumentation) ; on activation `on-start` + `on-tick` fire;
   on switch-away tasks cancel (no late write, version-stamped keys).
7. **Boot reseed with programs.** Restart with an active validated exec
   scene: programs installed from `execForAir`, `on-start` fires, logic
   live — asserted E2E.
8. **Deferred swap on validation success** (completes ADR 003 #15): active
   scene, non-validated re-push (antenna unmoved) → `/validate` succeeds →
   swap proceeds (`scene_changed` + snapshot) without a second push.
9. **Conformance end-state.** `conformance_allowlist.txt` is **empty**; the
   matrix exempts the 6 `core.db.*` atoms as inline-only with reason; the
   ratchet stays armed.
10. **Prerequisite gate.** The §3.4 PR carries verified links: P-1…P-4 done,
    P-5 confirmed (or the deployment-policy hold documented). Vigil refuses
    the merge otherwise.

---

Refs: ADR 003 (accepted) §3.1–§3.4, Amendment 1, criteria #2/#4/#15/#20 ;
issues #82, #83, #86, #87, #88, #89 ; `compiler/graph.go:36-46`,
`compile.go:509-547`, `runtime/show.go:107-153`, `runtime/exec.go`,
`runtime/validation_harness.go:52-72`, `api/gate.go`, `tick.go:78-89` ;
`Blue/src/blue/services/node_purity.py`.
