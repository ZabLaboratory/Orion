# Runbook — M3: named leaderboard (static unroll: 5 sequential truth queries)

> The executable sequence to take the **Milestone 3** named-leaderboard
> scene to air. M2 put a top-5 `player_scores` ranking on the antenna as
> raw `player_id` UUIDs + scores. M3 resolves each UUID to a **human
> pseudo** by querying **ZabTruth**'s `players` table — and renders a
> multi-line `"1. <pseudo> — <score>\n2. …"` board. It keeps the M1
> reactive chat guard. Owner: **Keeper** executes on `vps-ovh`; **Forge**
> produced. Surface: gateway-only via ZabGate (`https://zabgate.cyell.dev`),
> operator-authenticated.
>
> Companion artefacts (sources of truth, kept honest by CI):
> - `Blue/tests/fixtures/m3_named_leaderboard.py` +
>   `Blue/docs/runbooks/m3-named-leaderboard-bp-graph.json` — the
>   `bp-live-data-named` graph (test: `Blue/tests/test_m3_named_leaderboard.py`).
> - `ZabCanvas/tests/test_m3_named_leaderboard_layout.py` — the Canvas
>   layout bundle + its content hash (the `$CANVAS_VERSION` pinned below).
> - `Orion/internal/runtime/named_leaderboard_test.go` — the DB-free flight
>   proof (compile → install → fire on-start → 5 sequential truth queries →
>   folded board). `Orion/tests/e2e/named_leaderboard_test.go` — the full
>   e2e (push → validate → activate → real `_query`; + reactive chat; + R9).

## 0. The third way (read first)

A dynamic `player_id → pseudo` join is **not expressible** in the engine
(no string-equal, no runtime get-field-path, and a per-row query in a
`for-loop` forks out of order). The top-5 is a **fixed shape**, so the
graph spells it out — the **static unroll**, zero loop:

```
EXEC spine (strictly sequential — each db.query parks/resumes before the next):
  on-start ─→ db.query(ranking, top-5) ─→ variable.set(ranking_rows)
           ─→ query0(truth, id_0) ─→ variable.set(row_0)
           ─→ query1(truth, id_1) ─→ variable.set(row_1)
           ─→ …
           ─→ query4(truth, id_4) ─→ variable.set(row_4)
           ─→ variable.set(leaderboard_display = "1. …\n2. …\n…")
  → the multi-line board lands on  __vars..leaderboard_display

per rank k (pure cone, demand-pulled):
  id_k    = get-field(ranking_rows, path="k.player_id")   # STATIC config path
  score_k = get-field(ranking_rows, path="k.score")
  desc_k  = {table:"players", select:["display_name","summoner_name"],
             where:[{column:"id", op:"=", value:id_k}]}    # built from data
  name_k  = get-field(row_k, path="0.summoner_name")   # NOT NULL; LoL in-game name
  line_k  = concat(concat(concat("<k+1>. ", name_k), " — "), to-string(score_k))

DATAFLOW tranche (M1 reactive guard — unchanged):
  quasar.twitch.chat(g2nmathias) → … → output("chat.display")
```

The six `db.query` nodes are **exec-bearing**, so the scene goes through
the **#87 validation gate**: an unvalidated scene fires **no** query
(proven by `TestE2E_NamedLeaderboard_UnvalidatedSceneNeverQueries` and the
R9 half of the runtime proof).

### Three engine-shape facts (why the graph is wired this way)

1. **`op` is `"="`** — the QueryMe `Operator` literal (not `"eq"`;
   `QueryMe/src/queryme/descriptor.py`). The `WhereClause` is `extra="forbid"`,
   so the descriptor carries exactly `{column, op, value}`.
2. The descriptor `value` is spliced from data: literal `{column:"id",op:"="}`
   → `set-field(path="value", value=id_k)` → `list-append([], clause)` →
   `set-field(players_base, path="where", value=[clause])`. (`set-field`
   walks only object intermediates, so the `where` LIST is built by
   `list-append`, not by indexing into it.)
3. `ranking_rows` and each `row_k` are read back through a **`core.input`**
   node whose `config.name` IS the `__vars..<var>` leaf the exec
   `variable.set` wrote — **not `core.variable.get`**, which has no
   data-layer leaf and never resolves in the pure demand cone.

### Pseudo column (the M3b fix — content note)

The displayed name reads **`summoner_name`** — the LoL in-game name,
**NOT NULL** in ZabTruth's `players` table — NOT `display_name`. The real
top-5 carry a NULL `display_name` (only `summoner_name` is filled:
`GIDEON, Teddy, Loki, Gumayusi, Namgung`), which previously rendered blank
lines. The descriptor still selects BOTH columns. The engine has no
value-coalesce op, so a pure-graph `display_name ?? summoner_name` is not
expressible (flagged to Atlas) — but `summoner_name` is the correct,
robust choice for a LoL leaderboard regardless, and needs no seeding.

### Two values that are NOT literals (the canary lesson)

> ⚠️ **`blue_blueprint_id` is the Blue blueprint's UUID, NOT the slug
> `"bp-live-data-named"`.** §2 creates it; record the UUID as `$BP_UUID`.

> ⚠️ **`canvas_version` is the sha256 layout hash from ZabCanvas, NOT a
> literal.** Pinned below (§1), reproduced by
> `ZabCanvas/tests/test_m3_named_leaderboard_layout.py`.

### Prerequisites (deployment, already wired)

- `ORION_DATASOURCES` (étage-1 `.env.orion`) declares **both**
  `ranking=ranking` AND `truth=truth`. An undeclared datasource fails
  compile/run with `DATASOURCE_NOT_DECLARED` (structural, never a
  capability refusal). The five truth queries need `truth`.
- Orion's service token carries `query.read.ranking` AND
  `query.read.truth` (P2). The db.query effect wiring is merged + active.
- ZabTruth's `_query` catalog exposes `players` (`id`, `summoner_name`,
  `display_name`; `ZabTruth/src/zabtruth/services/db_catalog.py`).
- Quasar is connected to `g2nmathias` for the reactive guard.

## 1. Shell setup

```bash
export GW="https://zabgate.cyell.dev"
export SCENE_ID="$(uuidgen)"          # the Orion scene UUID — record it
# OP_TOKEN: operator/admin bearer, exported in the operator shell only.
# Never paste it into this file, the incident log, or a command echo.

# The M3 Canvas layout content hash (pinned; see §3).
export CANVAS_VERSION="a314ea8a0dbcb04317d53c392507ab8bf2467dfa6e91d09ad8d210503369aa2c"
```

> `jq` is absent on the host — payloads go via stdin (`-d @-`), never a
> `/tmp` file. Where a body is small and fixed it is inlined.

## 2. Author `bp-live-data-named` in Blue (returns the UUID)

```bash
# 2.1 Create the blueprint — kind "workflow" (exec-bearing: db.query).
curl -fsS -X POST "$GW/blue/api/v1/blueprints" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d '{
        "slug": "bp-live-data-named",
        "name": "M3 named leaderboard scene",
        "kind": "workflow",
        "tags": ["m3","live","data","ranking","truth","reactive"]
      }'
# 201 → record the returned "id" as BP_UUID. Seeds an empty DRAFT v1.
export BP_UUID="<id from the 201 response>"

# 2.2 Write the graph onto draft v1 (graph file next to this runbook on the
# Blue repo — Blue/docs/runbooks/m3-named-leaderboard-bp-graph.json).
curl -fsS -X PUT "$GW/blue/api/v1/blueprints/$BP_UUID/versions/1" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  --data-binary @- <<JSON
{ "graph": $(cat m3-named-leaderboard-bp-graph.json) }
JSON
# 200. Advisory findings on the db.query nodes' Orion-local exec ports are
# EXPECTED and NON-FATAL — Blue returns findings, never raises; Orion
# fetches the raw graph and partitions on its own ports. Do NOT block.

# 2.3 Publish v1 (sets current_version = 1).
curl -fsS -X POST "$GW/blue/api/v1/blueprints/$BP_UUID/versions/1/publish" \
  -H "authorization: Bearer $OP_TOKEN"
# 200 → version 1 frozen; Orion's FetchBlueprint resolves id → v1 → graph.
```

## 3. Store the Canvas layout (content-addressed)

The M3 layout is a vertical stack of two text elements: a multi-line
leaderboard element bound to `__vars..leaderboard_display` (style
`whiteSpace: pre-line` so the `\n` rank separators render as breaks) and a
chat element bound to `chat.display`. Its content address is
`$CANVAS_VERSION` (pinned in §1).

```bash
# Regenerate the exact bundle the test pins (from the ZabCanvas repo root):
python - <<'PY' > m3-layout-bundle.json
import json, sys
sys.path.insert(0, "tests")
from test_m3_named_leaderboard_layout import _named_leaderboard_bundle
print(json.dumps(_named_leaderboard_bundle()))
PY
# scene_version == sha256:a314ea8a… (== $CANVAS_VERSION).

# PUT it (idempotent; 201 first time, 200 if already stored).
BUNDLE="$(cat m3-layout-bundle.json)"
ARCHIVE_B64="$(printf '%s' "$BUNDLE" | base64 -w0)"
curl -fsS -X PUT "$GW/canvas/api/v1/lsml-bundles/$CANVAS_VERSION" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  --data-binary @- <<JSON
{ "bundle": $BUNDLE, "archive": "$ARCHIVE_B64" }
JSON

# Confirm Orion's compile step 1 resolves the layout (two bound elements):
curl -fsS "$GW/canvas/api/v1/layouts/$CANVAS_VERSION" \
  -H "authorization: Bearer $OP_TOKEN"
# root.children[*].bindings.text == ["__vars..leaderboard_display", "chat.display"].
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
# air unvalidated → none of its six db.query effects can fire (R9).
```

## 6. Validate, then poll until validated

```bash
curl -fsS -X POST "$GW/orion/api/v1/scenes/$SCENE_ID/validate" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" -d '{}'
# 202 Accepted. In validation mode every db.query resolves to a synthetic
# {rows:[],count:0} and NEVER touches ZabRanking/ZabTruth — the gate proves
# termination (all six effects park/resume) without a live query.

until curl -fsS \
  "$GW/orion/api/v1/scenes/$SCENE_ID/validation?v=$SCENE_VERSION" \
  -H "authorization: Bearer $OP_TOKEN" | grep -q '"status":"validated"'
do sleep 1; done
echo "m3 named-leaderboard scene validated"
```

## 7. Rollback prep (pick the fallback BEFORE going on air)

```bash
curl -fsS "$GW/orion/api/v1/show" -H "authorization: Bearer $OP_TOKEN"
export ROLLBACK_ID="<uuid of a healthy, validated scene>"   # e.g. the M2 scene
```

## 8. Activate — the scene goes live (the six queries fire in order)

```bash
curl -fsS -X POST "$GW/orion/api/v1/show/active-scene" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"scene_id\":\"$SCENE_ID\"}"
# Expect 200. on-start fires on air → the ranking query lands the top-5,
# then five SEQUENTIAL truth queries resolve each player_id to a pseudo,
# then the folded board lands on __vars..leaderboard_display. The scene
# also declares the chat leaf, so Quasar's next chat write migrates in.
```

## 9. Observe the franchissement (the proof)

| Signal | Where | Healthy value |
|---|---|---|
| **`__vars..leaderboard_display`** | live snapshot `state` | a multi-line string `"1. <pseudo> — <score>\n2. <pseudo> — <score>\n…"` (5 ranks), the real ZabTruth pseudos mapped onto the real ZabRanking scores |
| **`chat.display`** | live snapshot `state` | `"<author>: <message>"` for the last Twitch chat message (send one in `#g2nmathias`); repaints reactively (the M1 guard) |

The load-bearing proof: `__vars..leaderboard_display` is a **named**
leaderboard — five player_ids resolved to pseudos through five **sequential
truth queries** — AND `chat.display` reflects a **real Twitch chat event**.

> If `__vars..leaderboard_display` shows blank pseudos (`"1.  — 9"`): the
> five players lack a `summoner_name` in ZabTruth — but `summoner_name` is
> NOT NULL (§0), so this should not happen; check the catalog actually
> returns it. If it stays `""` entirely: check (a) `ORION_DATASOURCES`
> declares `truth=truth`, (b) Orion's token carries `query.read.truth`
> (else ZabTruth 403 → the error halts that rank's chain), (c)
> `/truth/health` 200 via ZabGate. A failed query halts-at-node (loudly
> logged); the renderer degrades gracefully.
> If `chat.display` stays empty: the M1 troubleshooting in
> `Blue/docs/runbooks/m1-reactive-chat.md` §9 applies verbatim.

## 10. Rollback (if the scene misbehaves)

```bash
curl -fsS -X POST "$GW/orion/api/v1/show/active-scene" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"scene_id\":\"$ROLLBACK_ID\"}"
# Single active-scene switch — no recompile, no deploy. The M3 scene stays
# pushed/validated for post-mortem; it is simply off air.
```

## 11. Post-flight

- Record `$SCENE_ID`, `$BP_UUID`, `$CANVAS_VERSION`, `$SCENE_VERSION`, the
  observed `__vars..leaderboard_display` board and `chat.display` line, and
  whether rollback was used (never the token).
- `bp-live-data-named` (published v1) and the scene may stay as the standing
  M3 named-leaderboard scene, or be archived per Keeper's call.
