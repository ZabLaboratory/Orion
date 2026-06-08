# ADR 002 — Scene lifecycle Canvas↔Orion (upsert-on-push, name source of truth)

- **Status**: accepted
- **Date**: 2026-06-08
- **Decided**: 2026-06-08
- **Deciders**: @ClodoCapeo (maintainer), Vigil (review)
- **Author**: Atlas (architect agent)
- **Supersedes**: —
- **Superseded by**: —

---

> **Why this ADR lives in `Orion/docs/adr/` and is numbered 002.** The contract
> gap is owned by Orion: the `scenes` row, the `scene_definitions.scene_id` FK,
> and `pushScene`'s `GetScene` precondition all live in this repo
> (`migrations/0001_init.sql`, `internal/api/scenes_push.go`,
> `internal/store/scenes.go`). 001 established the per-repo convention; this is
> the next free number. The Canvas-side error-mapping fix is a **consequence**,
> cross-referenced (§3.4), not a separate ADR — it carries no independent
> architectural choice, it mirrors the error vocabulary Orion already emits.
> Note (verified, same as ADR 001 §header): `docs/adr/003` (save/push split) and
> `docs/adr/004` (v2 runtime) are **referenced in code/CLAUDE.md but do not exist
> on disk** — they are design references, not committed artefacts. There is
> therefore no 004 to amend; this is a **new ADR**, not an amendment.

## 1. Context

Test M8 (the maximal live-test path, ADR 002-Pulsar Amendment 1 / ADR 001) is the
first end-to-end driver of the production push chain and surfaced a hard contract
gap (Conduit's investigation, **B1**):

**No production code creates the `scenes` row inside Orion's database.**

- **Canvas** owns the authoring lifecycle and creates the scene in **its own** DB
  (`ZabCanvas` `scenes` table, see Canvas `CLAUDE.md`). It never tells Orion "a
  scene with this id now exists."
- **Orion** has `Store.CreateScene` (`store/scenes.go:67`) but **no route calls it
  in prod** — it is exercised only by the e2e fixtures.
- At push, `pushScene` (`scenes_push.go:36`) does `GetScene(sceneID)` as a
  precondition. On a scene Canvas created but never seeded into Orion, this returns
  `store.ErrNotFound` → `codeFromError` maps it to **`404 NOT_FOUND`**
  (`public.go:152-153`). The push is rejected before any compile.

There is a **second, structural** reason the row must exist that Conduit's note did
not call out and this ADR records: `scene_definitions.scene_id` is a
**FK `REFERENCES scenes(id)`** (`0001_init.sql:27`). Even if the `GetScene`
precondition were removed, `InsertDefinition` (`scenes_push.go:89`) would fail on
the FK for a never-seeded scene. So creating the `scenes` row is **mandatory**, not
merely a 404-avoidance nicety.

The intent that the **first push makes a scene runnable** is already encoded:
`CreateScene` inserts with `latest_pushed_version = NULL`
(`scenes.go:65-66` comment "The first push is what makes the scene runnable"), the
column is `NULL until first push` (`0001_init.sql:16`), and
`ListActiveScenesWithPush` (`scenes.go:88`) only revives scenes with a non-null
pointer. The lifecycle was designed for "row exists, pointer null, then push fills
it" — but the **row-creation step was never wired** to any prod caller.

### 1.1 The `name` constraint

`scenes.name` is `text NOT NULL` (`0001_init.sql:13`). The push envelope
(`compiler.PushEnvelope`, ADR 001 §3.1) carries **no `name`** — it is a compile
input (canvas_version, blueprints, components, lsml hash, rollback). So any
auto-create at push must supply a `name` value. The canonical, human-meaningful
name lives in **Canvas** (`ZabCanvas.scenes.name`), which Orion has no reason to
duplicate as source of truth.

### 1.2 The sibling gap: active-scene

`POST /show/active-scene` (`show.go:30`) has the **same class** of gap: it is the
production way to flip the live scene, but **no service calls it in prod**. The only
repo reference to "active-scene" outside Orion is `Prism/src/main/broadcast-url.ts`,
which is a **URL builder, not a caller**. M8 is the first driver of this endpoint
too. Unlike B1 (which the normal push path silently 404s through), active-scene is
not silently broken — it is simply **never invoked**: there is no wiring, not a bad
contract. §3.5 decides its scope here.

## 2. Decision drivers

- **The normal push path must not 404 on a first push.** A scene Canvas created and
  the operator pushes for the first time is the *expected* path, not an error.
- **Idempotent and torn-state-free.** Push already persists in one transaction
  (`scenes_push.go:127`). Scene creation must not introduce a race where two
  concurrent first-pushes both try to insert, or where the row exists without the
  FK target being visible to `InsertDefinition`.
- **No new surface, no new contract, smallest contract change (Conduit's a≺b≺c).**
  Conduit rejected (b) Canvas-calls-create (extra round-trip, an inconsistency
  window between create and push) and (c) sync (more surface, drift). Auto-create
  on push (a) adds **zero new endpoints** and **zero new producer obligations** —
  the producer keeps doing exactly one call (`POST …/push`).
- **Single source of truth for `name`.** Canvas owns the canonical name; Orion must
  not become a second authority that can drift. Orion's copy is a runtime/debug
  convenience, not the truth.
- **Fail-closed error vocabulary across the seam.** A genuine "scene truly does not
  exist" must still surface a code Canvas can map, not a generic 502.

## 3. Decision

**Go.** Orion **auto-creates the `scenes` row on push if absent**, idempotently,
inside the existing push transaction. `name` is a **placeholder** (`name = scene_id`)
— Canvas remains the source of truth for the canonical name. The `404 NOT_FOUND`
disappears from the normal push path. Active-scene (§1.2) is **scoped out** as a
future wiring task, explicitly tracked, not solved by code in this ADR.

### 3.1 Upsert-on-push (B1)

Replace the `GetScene`-as-precondition pattern with an **upsert before compile/
persist**:

| Step | Before (`scenes_push.go`) | After |
|---|---|---|
| Precondition | `GetScene` → 404 `NOT_FOUND` if absent | `UpsertScene(id, placeholderName)` — creates if absent, no-op if present |
| Archived guard | `if scene.Status == SceneArchived → 409 SCENE_ARCHIVED` | **preserved** — upsert returns the row (existing or freshly-created `active`); the archived check runs on the returned row |
| FK target | implicit, assumed present | guaranteed present before `InsertDefinition` |

New store method (`store/scenes.go`):

```go
// UpsertScene ensures a scenes row exists for id. On first push it
// creates the row (status=active, latest_pushed_version=NULL, name=
// placeholder). On a subsequent push it is a no-op and returns the
// existing row unchanged (existing name/status/pointer preserved).
// Idempotent under concurrent first-pushes via ON CONFLICT.
func (s *Store) UpsertScene(ctx context.Context, id uuid.UUID, name string) (*Scene, error) {
    row := s.pool.QueryRow(ctx,
        `INSERT INTO scenes (id, name, status) VALUES ($1, $2, 'active')
           ON CONFLICT (id) DO UPDATE SET id = scenes.id
           RETURNING id, name, status, latest_pushed_version, created_at, updated_at`,
        id, name)
    return scanScene(row)
}
```

**`DO UPDATE SET id = scenes.id`** (a no-op write to the PK) rather than
`DO NOTHING`, deliberately: `ON CONFLICT … DO NOTHING` does **not** return a row
via `RETURNING`, so a concurrent first-push that loses the insert race would get
zero rows and have to re-`SELECT`. The no-op `DO UPDATE` makes `RETURNING` always
yield the surviving row in one statement — idempotent, race-safe, one round-trip.
It **does not overwrite `name`, `status`, or `latest_pushed_version`** on an
existing scene (only `id` is touched, to itself).

**Transaction placement.** The upsert runs **first**, before `Compile`, so the FK
target exists and the archived guard can run on the real row. It is acceptable for
the upsert to be its own statement before the persist `Tx` block (a scene row with
no definitions yet is a benign intermediate state — `ListActiveScenesWithPush`
ignores it because `latest_pushed_version IS NULL`). Forge **may** fold the upsert
into the existing persist transaction if straightforward; it is not required for
correctness because the upsert is idempotent.

### 3.2 The `name`: placeholder, Canvas is source of truth (DECIDED)

**Decision: placeholder `name = scene_id`.** The envelope is **not** extended with
an optional `name`.

Rationale (this is the one place Conduit left to Atlas to trench):

- The canonical name lives in **Canvas** (`ZabCanvas.scenes.name`). Orion's `name`
  is used only for operator-facing debug (`getShow`/roster). Making Orion store a
  *copy* fed from the envelope creates a **drift surface**: rename in Canvas, and
  Orion's copy goes stale unless every rename re-pushes. That is a worse contract
  than an honest placeholder.
- Extending the envelope with `name` would also mean the `name` becomes a **compile
  input** living next to `canvas_version`/`blueprints`, implying it participates in
  the push identity — it must **not** (renaming a scene must not change its
  `scene_version`). Keeping it out of the envelope keeps `scene_version` purely a
  function of compiled content (ADR 001 §3.5 determinism).
- `scene_id` as the placeholder is always present, unique, and NOT-NULL-satisfying.
  It is never shown as a user-facing label in any surface that matters (Prism reads
  the name from Canvas, not Orion).

**Consequence for the future:** if a real need appears for Orion to show the
human name (e.g. an Orion-native operator console), the correct fix is a **dedicated
metadata channel** (Canvas → Orion name sync, or Orion reads Canvas), authored in a
follow-up ADR — **not** smuggling it through the compile envelope. Recorded as a
risk (R3), not built now.

### 3.3 The `NOT_FOUND` code on push

With upsert-on-push, **the push handler no longer returns `NOT_FOUND` for a missing
scene** — the row is created on demand. The generic `store.ErrNotFound → NOT_FOUND`
mapping in `codeFromError` (`public.go:152`) **stays** for the other Get-paths that
legitimately 404 (render-bundle by version, rollback target, etc.), but it is
**off the push happy/first-push path** entirely. No code is removed from
`codeFromError`; the push handler simply stops reaching the `ErrNotFound` branch
for the "scene row absent" reason.

### 3.4 Canvas error-mapping drift (consequence, repo ZabCanvas)

Independently of B1, Conduit found and this ADR records a **latent** drift:
`orion_client.py` documents/anticipates `SCENE_NOT_FOUND` (docstring line 75) as
the missing-scene code, but Orion's generic store-miss emits **`NOT_FOUND`**
(`public.go:153`), and the runtime-miss emits `SCENE_NOT_FOUND` (`public.go:155`).
The two are distinct codes. When Orion returns `NOT_FOUND`, Canvas's client does not
recognise it as a scene-absence and surfaces a generic failure (toward a **502** at
the gateway boundary per Conduit), masking the cause.

After §3.1 this no longer fires on the push first-push path (the row is auto-created),
so it is **lower severity** — but the mapping is still wrong for any genuine
`NOT_FOUND` Orion may return on push (e.g. archived-after-check edge, rollback
target miss surfaced through the push route). **Fix: Canvas `orion_client` maps
both `NOT_FOUND` and `SCENE_NOT_FOUND` to its scene-absence error class**, so the
cause is preserved rather than collapsed into a generic 502. Owned by the Canvas
repo, tracked as its own issue (§7).

### 3.5 Active-scene gap (§1.2): scoped OUT, tracked (DECIDED)

**Decision: this ADR does NOT solve active-scene; it records it as a wiring gap and
tracks it.** Rationale:

- B1 is a **broken contract** (the normal push path errors). Active-scene is an
  **unwired-but-correct** endpoint: `POST /show/active-scene` works as specified
  (`show.go:30`, criterion 2 `SCENE_NOT_PUSHED` enforced) — no service simply calls
  it yet. There is **no contract to fix**, only a producer to write.
- Deciding *who* drives active-scene (Prism operator UI? Canvas go-live? Pulsar
  show control?) is a **separate product/wiring decision** that does not belong in a
  data-lifecycle ADR and would force premature coupling.
- M8 can drive `POST /show/active-scene` **directly** as its test harness does today
  — it does not need a prod producer to validate the live path.

**Tracked as:** a follow-up wiring item — "decide and wire the prod driver of
`POST /show/active-scene` (Prism go-live or Canvas)." Flagged to Eleven as a product
decision (§7), not converted to a build issue in this ADR.

## 4. Consequences

- **The normal first-push path returns 200** and creates the runnable scene row —
  M8's B1 wall is removed. No new endpoint, no new producer obligation: the producer
  still makes exactly one `POST …/push` call.
- **Zero migration.** No schema change — `UpsertScene` uses the existing
  `scenes(id)` PK and the existing nullable `latest_pushed_version`. `CreateScene`
  stays for the e2e fixtures (or Forge may have `UpsertScene` supersede it; not
  required).
- **`name` stays single-source (Canvas).** Orion carries a `scene_id` placeholder;
  no drift surface, no envelope bloat, `scene_version` stays content-only.
- **Canvas error mapping becomes honest** — a real scene-absence is no longer
  masked as a generic 502.
- **Active-scene remains a known, tracked wiring gap** — not silently forgotten, not
  prematurely solved.
- `CreateScene`'s prod-dead status is resolved: either replaced by `UpsertScene` or
  left as e2e-only with a comment pointing here.

## 5. Risks

Security-surfaced risks → **Bastion** (do not self-clear).

- **R1 — Push auto-provisions DB rows (resource creation on a write path).** Any
  authorised operator push now creates a `scenes` row for **any** UUID it names,
  even one Canvas never authored. Today `pushScene` is `requireOperator`-gated
  (`scenes_push.go:21`), so this is operator-only, not anonymous — bounded. But it
  means Orion's `scenes` table can diverge from Canvas's (a scene exists in Orion
  that Canvas does not know). **Residual, accepted:** operator-gated, and an
  orphan Orion scene with `latest_pushed_version` set is still a real runnable
  scene (the operator did push content to it) — not garbage. → **Bastion**: confirm
  no unbounded-creation / id-enumeration concern on the operator-gated push surface
  (no new auth primitive, same gate as today).
- **R2 — Concurrent first-push race.** Two simultaneous first-pushes of the same id
  must not both insert or tear. Mitigated by `ON CONFLICT (id) DO UPDATE … RETURNING`
  (§3.1) — atomic, returns the surviving row, idempotent. **Must be covered by the
  contract test** (criterion 6.4) before merge.
- **R3 — `name` placeholder leaks into a user-facing surface.** If some surface ever
  renders Orion's `name`, operators see a raw UUID. Mitigated: no current surface
  does (Prism reads Canvas). Accepted; the proper fix (name sync) is a future ADR
  (§3.2), explicitly **not** the compile envelope.
- **R4 — Active-scene stays unwired indefinitely.** Scoping it out (§3.5) risks it
  being forgotten. Mitigated by the tracked follow-up (§7) and this explicit record.
  Accepted (it does not block M8, which drives the endpoint directly).

No new auth primitive, no new network surface (same operator-gated push endpoint,
no new route). No secret handling change.

## 6. Resolution criteria

Testable, aligned with Orion's `CLAUDE.md` gates and the M-series convention.

1. **First-push creates the row.** `POST /scenes/{id}/push` on a scene whose row
   **never existed in Orion** returns **200** (not 404), and a `scenes` row now
   exists with `status='active'`, `name = {id}`, and `latest_pushed_version` set to
   the new `scene_version`. (Contract test — Probe.)
2. **No 404 on the first-push path.** The same push asserts the response code is
   **never** `NOT_FOUND` for the "row absent" reason.
3. **Idempotent re-push preserves the row.** A second push of the same scene does
   **not** reset `name`, `status`, or overwrite an operator-set name; it advances
   `latest_pushed_version` exactly as today (criterion 1 of ADR 004 §12 unbroken).
4. **Concurrent first-push is race-safe.** Two concurrent first-pushes of the same
   id both succeed (or one upserts and one no-ops), exactly one row exists, no
   `duplicate key` / FK error surfaces. (R2.)
5. **Archived guard preserved.** Pushing to a scene whose row exists with
   `status='archived'` still returns **409 `SCENE_ARCHIVED`** (the upsert does not
   resurrect an archived scene — `DO UPDATE SET id=id` does not touch `status`).
6. **FK satisfied.** `InsertDefinition` on a first-push scene succeeds (the FK
   `scene_definitions.scene_id → scenes(id)` is satisfied because the upsert ran
   first).
7. **Canvas error mapping (ZabCanvas repo).** `orion_client` maps **both**
   `NOT_FOUND` and `SCENE_NOT_FOUND` to its scene-absence error class; a unit test
   feeds an Orion `404 {code: NOT_FOUND}` body and asserts it is **not** collapsed
   into a generic/502 error. `mypy --strict` + `ruff` green.
8. **`scene_version` unchanged by lifecycle.** The `scene_version` produced for a
   given compiled content is **identical** whether the scene was auto-created on
   this push or pre-existed — `name`/lifecycle never enter the hash (ADR 001 §3.5
   determinism unbroken).
9. **Org gates.** Orion CI green (vet/test/build/staticcheck/golangci/trufflehog);
   Canvas CI green (ruff/mypy/pytest); review approved by **Vigil**; **Conduit**
   smoke-validates the transverse push (Canvas → ZabGate → Orion, first-push →
   200 + row created); **Bastion** clearance on R1 (operator-gated auto-create, no
   new surface).
