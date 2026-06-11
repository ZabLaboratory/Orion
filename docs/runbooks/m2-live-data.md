# Runbook — M2: live data scene (reactive chat + real `db.query` to air)

> The executable sequence to take the **Milestone 2** live-data scene to
> air: the first scene that is BOTH reactive (a real Twitch chat message
> repaints a Canvas text element live — the M1 guard) AND data-bearing (an
> exec `db.query` against **ZabRanking** returns real rows — a top-5
> player-grade leaderboard — that land on the antenna). This is the
> **`+effets data`** criterion. Owner: **Keeper** executes on `vps-ovh`;
> **Forge** produced. Surface: gateway-only via ZabGate
> (`https://zabgate.cyell.dev`), operator-authenticated.
>
> Companion artefacts (sources of truth, kept honest by CI):
> - `Blue/tests/fixtures/m2_live_data.py` + `Blue/docs/runbooks/m2-live-data-bp-graph.json`
>   — the `bp-live-data` graph (test: `Blue/tests/test_m2_live_data.py`).
> - `ZabCanvas/tests/test_m2_live_data_layout.py` — the Canvas layout
>   bundle + its content hash (the `$CANVAS_VERSION` pinned below).
> - `Orion/tests/e2e/live_data_test.go` — the e2e proof (push → validate →
>   activate → on-start fires db.query → rows on `__vars..ranking_rows`;
>   + the reactive chat guard; + R9 negative: unvalidated never queries).

## 0. What flies (read first)

`bp-live-data` carries TWO tranches in one blueprint:

```
EXEC tranche (the data core — exec-bearing, gated by validation #87):
  on-start ─then→ db.query(datasource="ranking", descriptor=<top-5>)
                    └ then ──→ variable.set(ranking_rows = q.rows)
  → the rows land on the leaf  __vars..ranking_rows  (Canvas renders them)

DATAFLOW tranche (the M1 reactive guard — pure dataflow):
  quasar.twitch.chat(g2nmathias) ── payload ─┬─ get-field("actor.display_name") ─┐
                                             └─ get-field("payload.text") ───────┤
      concat(author, ": ") ──► concat(authorSep, text) ──► output("chat.display")
  → the line lands on the leaf  chat.display  (Canvas renders it)
```

The db.query node is **exec-bearing** (it carries exec pins), so the scene
goes through the **#87 validation gate** exactly like the canary: an
**unvalidated scene can never fire the query** (proven by
`TestE2E_LiveData_UnvalidatedSceneNeverQueries`). In validation mode the
db.query resolves to a synthetic `{rows:[],count:0}` and never touches the
gateway; on air it runs for real against `_query`.

### The `db.query` descriptor (the data effect)

A QueryMe `QueryDescriptor` — the top-5 player grades, highest first, off
ZabRanking's `player_scores` table (columns from
`ZabRanking/src/zabranking/services/db_catalog.py`):

```json
{
  "table": "player_scores",
  "select": ["player_id", "score"],
  "order": [{"column": "score", "direction": "desc"}],
  "limit": 5
}
```

Orion POSTs this verbatim to `${ZABGATE}/ranking/api/v1/_query` with its
own service token; ZabRanking compiles it (QueryMe — read-only,
parameterized) and runs it against its DB. The token MUST carry the scope
`query.read.ranking` (asserted by ZabRanking, `internal.py::require_query_scope`).

> **Content note (ajustable).** Top-5 raw player_scores is the demo default
> Forge chose to make the data path visible on air. `player_id` shows as a
> UUID; resolving it to a display name is a ZabTruth join across a service
> boundary (not expressible in one `_query` — gateway-first) and is left
> for a content pass. Swap the descriptor (e.g. a single-player stat, or a
> split-averages query) without touching the blueprint shape if the porteur
> wants different content.

### Two values that are NOT literals (the canary lesson)

> ⚠️ **`blue_blueprint_id` is the Blue blueprint's UUID, NOT the slug
> `"bp-live-data"`.** §2 creates the blueprint; record the UUID Blue
> returns as `$BP_UUID`.

> ⚠️ **`canvas_version` is the sha256 layout hash from ZabCanvas, NOT a
> literal.** It is **pinned** below (§3) — the canonical `hashBundle` of
> the M2 layout, reproduced by `ZabCanvas/tests/test_m2_live_data_layout.py`.

### Prerequisites (deployment, already wired)

- `ORION_DATASOURCES` (étage-1 `.env.orion`) declares `ranking=ranking`
  (and `truth=truth`). An undeclared datasource fails compile/run with
  `DATASOURCE_NOT_DECLARED` — structural, never a capability refusal.
- Orion's service token carries `query.read.ranking` (P2). The db.query
  effect wiring is merged + active (#126).
- Quasar is connected to `g2nmathias` (M1 prereq, PR #124) for the reactive
  guard.

## 1. Shell setup

```bash
export GW="https://zabgate.cyell.dev"
export SCENE_ID="$(uuidgen)"          # the Orion scene UUID — record it
# OP_TOKEN: operator/admin bearer, exported in the operator shell only.
# Never paste it into this file, the incident log, or a command echo.

# The M2 Canvas layout content hash (pinned; see §3).
export CANVAS_VERSION="f6c5b3b7fa46d10fe8ecad7f65a4944e51d2dc03bb9d03b36fabd06cb91143b4"
```

> `jq` is absent on the host — payloads go via stdin (`-d @-`), never a
> `/tmp` file. Where a body is small and fixed it is inlined.

## 2. Author `bp-live-data` in Blue (returns the UUID)

```bash
# 2.1 Create the blueprint — kind "workflow" (exec-bearing: db.query).
curl -fsS -X POST "$GW/blue/api/v1/blueprints" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d '{
        "slug": "bp-live-data",
        "name": "M2 live data scene",
        "kind": "workflow",
        "tags": ["m2","live","data","ranking","reactive"]
      }'
# 201 → record the returned "id" as BP_UUID. Seeds an empty DRAFT v1.
export BP_UUID="<id from the 201 response>"

# 2.2 Write the graph onto draft v1 (graph file next to this runbook —
# ../../Blue/docs/runbooks/m2-live-data-bp-graph.json on the Blue repo).
curl -fsS -X PUT "$GW/blue/api/v1/blueprints/$BP_UUID/versions/1" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  --data-binary @- <<JSON
{ "graph": $(cat m2-live-data-bp-graph.json) }
JSON
# 200. Advisory findings on the db.query node's Orion-local exec ports
# (Blue seeds it as a data-only node) are EXPECTED and NON-FATAL — Blue
# returns findings, never raises; Orion fetches the raw graph and
# partitions on its own ports. Do NOT block on these (same as the canary).

# 2.3 Publish v1 (sets current_version = 1).
curl -fsS -X POST "$GW/blue/api/v1/blueprints/$BP_UUID/versions/1/publish" \
  -H "authorization: Bearer $OP_TOKEN"
# 200 → version 1 frozen; Orion's FetchBlueprint resolves id → v1 → graph.
```

## 3. Store the Canvas layout (content-addressed)

The M2 layout is a vertical stack of two text elements: a ranking element
bound to `__vars..ranking_rows` and a chat element bound to `chat.display`.
Its content address is `$CANVAS_VERSION` (pinned in §1).

```bash
# Regenerate the exact bundle the test pins (from the ZabCanvas repo root):
python - <<'PY' > m2-layout-bundle.json
import json, sys
sys.path.insert(0, "tests")
from test_m2_live_data_layout import _live_data_bundle
print(json.dumps(_live_data_bundle()))
PY
# scene_version == sha256:f6c5b3b7…43b4 (== $CANVAS_VERSION).

# PUT it (idempotent; 201 first time, 200 if already stored). archive is
# the base64 of the canonical bundle bytes.
BUNDLE="$(cat m2-layout-bundle.json)"
ARCHIVE_B64="$(printf '%s' "$BUNDLE" | base64 -w0)"
curl -fsS -X PUT "$GW/canvas/api/v1/lsml-bundles/$CANVAS_VERSION" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  --data-binary @- <<JSON
{ "bundle": $BUNDLE, "archive": "$ARCHIVE_B64" }
JSON

# Confirm Orion's compile step 1 resolves the layout (two bound elements):
curl -fsS "$GW/canvas/api/v1/layouts/$CANVAS_VERSION" \
  -H "authorization: Bearer $OP_TOKEN"
# root.children[*].bindings.text == ["__vars..ranking_rows", "chat.display"].
```

## 4. Push the scene (Orion)

```bash
curl -fsS -X POST "$GW/orion/api/v1/scenes/$SCENE_ID/push" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"canvas_version\":\"$CANVAS_VERSION\",\"blue_blueprint_id\":\"$BP_UUID\"}"
# 200 + {"scene_version":"sha256:…","diagnostics":{"errors":[],"warnings":[]}}.
export SCENE_VERSION="<scene_version from the response>"
# 422 COMPILE_FAILED → bp unpublished / wrong UUID / Blue unreachable.
# 422 FETCH_UPSTREAM "Layout not found" → CANVAS_VERSION not stored (run §3).
```

## 5. Gate proof — activate BEFORE validate is refused

```bash
curl -fsS -o /dev/null -w '%{http_code}' -X POST \
  "$GW/orion/api/v1/show/active-scene" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"scene_id\":\"$SCENE_ID\"}"
# Expect 409, body code SCENE_NOT_VALIDATED. The exec-bearing scene cannot
# air unvalidated → its db.query cannot fire (R9, the data effect is gated).
```

## 6. Validate, then poll until validated

```bash
curl -fsS -X POST "$GW/orion/api/v1/scenes/$SCENE_ID/validate" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" -d '{}'
# 202 Accepted. In validation mode db.query resolves to a synthetic
# {rows:[],count:0} and NEVER touches ZabRanking — the gate proves
# termination without a live query.

until curl -fsS \
  "$GW/orion/api/v1/scenes/$SCENE_ID/validation?v=$SCENE_VERSION" \
  -H "authorization: Bearer $OP_TOKEN" | grep -q '"status":"validated"'
do sleep 1; done
echo "m2 live-data scene validated"
```

## 7. Rollback prep (pick the fallback BEFORE going on air)

```bash
curl -fsS "$GW/orion/api/v1/show" -H "authorization: Bearer $OP_TOKEN"
export ROLLBACK_ID="<uuid of a healthy, validated scene>"   # e.g. the canary
```

## 8. Activate — the scene goes live (the db.query fires)

```bash
curl -fsS -X POST "$GW/orion/api/v1/show/active-scene" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"scene_id\":\"$SCENE_ID\"}"
# Expect 200. on-start fires on air → db.query POSTs the descriptor to
# /ranking/api/v1/_query with the service token → the rows resume onto
# __vars..ranking_rows. The scene also DECLARES the chat leaf, so Quasar's
# next chat write migrates into it (reactive guard).
```

## 9. Observe the franchissement (the proof)

| Signal | Where | Healthy value |
|---|---|---|
| **`__vars..ranking_rows`** | live snapshot `state` | a JSON array of up to 5 `{player_id, score}` rows, highest score first — the real ZabRanking data on air (the data effect) |
| **`chat.display`** | live snapshot `state` | `"<author>: <message>"` for the last Twitch chat message (send one in `#g2nmathias`); repaints reactively on each new message (the M1 guard) |

The load-bearing proof: `__vars..ranking_rows` carries **real ZabRanking
rows** AND `chat.display` reflects a **real Twitch chat event** — one scene,
reactive AND data-bearing, both on the antenna.

> If `__vars..ranking_rows` stays `null`: check (a) `ORION_DATASOURCES`
> declares `ranking=ranking`, (b) Orion's service token carries
> `query.read.ranking` (else ZabRanking 403 `MISSING_QUERY_SCOPE` → the
> error halts the chain, rows stay null), (c) ZabRanking `/ranking/health`
> is 200 via ZabGate. A failed query halts-at-node (loudly logged) and
> leaves the leaf at its seeded `null` — the renderer degrades gracefully.
> If `chat.display` stays empty: the M1 troubleshooting in
> `Blue/docs/runbooks/m1-reactive-chat.md` §9 applies verbatim.

## 10. Rollback (if the scene misbehaves)

```bash
curl -fsS -X POST "$GW/orion/api/v1/show/active-scene" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"scene_id\":\"$ROLLBACK_ID\"}"
# Single active-scene switch — no recompile, no deploy. The M2 scene stays
# pushed/validated for post-mortem; it is simply off air.
```

## 11. Post-flight

- Record `$SCENE_ID`, `$BP_UUID`, `$CANVAS_VERSION`, `$SCENE_VERSION`, the
  observed `__vars..ranking_rows` array and `chat.display` line, and whether
  rollback was used (never the token).
- `bp-live-data` (published v1) and the scene may stay as the standing M2
  live-data scene, or be archived per Keeper's call.
