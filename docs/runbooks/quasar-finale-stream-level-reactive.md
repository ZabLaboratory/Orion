# Runbook — Quasar finale: stream-level rule + reactive scene (live to air)

> The executable sequence to take the **Quasar finale** (ADR 013 issue 5)
> to air: a real Twitch chat message in `#g2nmathias` → a **stream-level
> Blue rule** fires `core.show.emit@1` → the **active reactive scene**
> reacts visually at the antenna. This proves `quasar.twitch.*` +
> `core.show.emit@1` + the stream-level (ADR 009) execution scope, end to
> end, on a real platform event.
>
> **Owner:** Keeper executes on `vps-ovh`; the live trigger (a chat message
> on `g2nmathias`) is done by the **porteur**. Surface: gateway-only via
> ZabGate (`https://zabgate.cyell.dev`), operator-authenticated.
>
> Companion artefacts (sources of truth, kept honest by CI):
> - `Blue/tests/fixtures/quasar_finale_stream_rule.py` +
>   `Blue/docs/runbooks/quasar-finale-stream-rule-bp-graph.json` — the
>   `bp-quasar-finale-stream-rule` graph
>   (test: `Blue/tests/test_quasar_finale_stream_rule.py`).
> - `Blue/tests/fixtures/quasar_finale_reactive_scene.py` +
>   `Blue/docs/runbooks/quasar-finale-reactive-scene-bp-graph.json` — the
>   `bp-quasar-finale-reactive-scene` graph
>   (test: `Blue/tests/test_quasar_finale_reactive_scene.py`).
> - `ZabCanvas/tests/test_quasar_finale_reactive_scene_layout.py` — the
>   Canvas layout bundle + its content hash (the `$CANVAS_VERSION` below).
> - Read `Zab/agents/_shared/live-testing.md` first — the leaf/Solar
>   pipeline, the `canvas_version` 64-hex contract, and the
>   `variable.set`-on-spine piège.

## 0. The two-scope contract (read first)

ADR 013 splits the finale into two execution scopes that meet at the
active scene's `__events.<topic>` leaf:

```
Twitch IRC ──► Quasar (normalizer → CanonicalEvent)
           ──► writes __inputs.platform.twitch.g2nmathias.last_chat   (service-token WS)

STREAM-LEVEL RULE  (bp-quasar-finale-stream-rule, PROMOTED, always-on)
  on-platform-event(twitch, g2nmathias, chat)        ← arms on that WRITE (ADR 013 §3)
    ──then──► show.emit(topic="stream_chat_event", payload=<chat event>)
              └─ EmitToActive: inject __events.stream_chat_event into the ACTIVE scene only
                 (ADR 009 §3.6 — never rule→rule, anti-loop by construction)

ACTIVE REACTIVE SCENE  (bp-quasar-finale-reactive-scene, ACTIVATED, on air)
  core.input("__events.stream_chat_event")   ← reads the emitted event leaf DIRECTLY (M1)
    value (DATA) ─► get-field("payload.text") ─► output("chat.display")   (DATAFLOW only)
              └─ leaf delta → LSDP → Solar repaints the text element live
```

> ⚠️ The reactive scene is **pure dataflow** — NO exec entry, NO exec
> spine, NO exec pins on any node. It reads the show event the rule emits
> (`__events.stream_chat_event`) with a `core.input@1` leaf node, exactly
> the proven M1 shape (`bp-m1-reactive-chat`) that read
> `__inputs.platform.*` reactively. `EmitToActive` writes the canonical
> event to that leaf in the active scene's state (it does NOT gate on
> `sceneAcceptsPath`), the runtime seeds the dirty cone on the write
> (`scene.go` `applyInput` → `s.pending[__events.stream_chat_event]`), and
> `recompute` wakes the get-field registered as the leaf's consumer
> (`upstreamPath` resolves the inbound edge to the input node's leaf, the
> `consumers` index). No compiler change is needed: a single-blueprint
> scene compiles under the legacy key `""`, so `prefixGraphNodes` leaves
> the `__events.*` leaf address unprefixed — byte-identical to the write.
>
> The **4th-link fix** (ADR 013 issue 4): the previous shape read
> `onChat.payload`, the DATA-OUT pin of an `on-event` exec entry. That pin
> lives only in the transient env of a fired exec task and is NEVER a state
> leaf; in a spine-less dataflow scene nothing on the dataflow side consumed
> `__events.stream_chat_event`, so the event reached state (deltas at the
> wire) but `chat.display` stayed `null`. Reading the leaf directly closes
> the link. Fixed in Blue `fix(finale): read __events leaf directly in
> dataflow` (authoring-only — no Orion compiler change).

The rule arms **all 14** canonical Twitch types (`chat`,
`subscription`, `subscription_gift`, `cheer`, `follow`, `raid`,
`stream_online`, `stream_offline`, `channel_point_redemption`,
`poll_begin`, `poll_end`, `prediction_begin`, `prediction_lock`,
`prediction_end`), each emitting on `stream_<type>_event`. The live proof
exercises **`chat`** (simplest to trigger); the other 13 share the
identical compiled path (same `on-platform-event` kind, same
`platformStreamBindings`, same `EmitToActive` injection).

### Conformance precondition (hard — verify before pushing)

Both ops MUST be in Orion's conformance matrix on the deployed build
(post-#170):

- `core.event.on-platform-event@1` → `ExecEntry{Kind:"on-platform-event"}`
- `core.show.emit@1` → exec op `show.emit`

If the deployed Orion predates #170, a push carrying either op fails with
`EXEC_OP_UNMAPPED` / an unmapped entry kind — STOP and redeploy Orion
first (`internal/conformance/conformance.go` must list both).

### Two values that are NOT literals (the canary lesson)

> ⚠️ `blue_blueprint_id` is each Blue blueprint's **UUID**, not the slug.
> §2 records the UUIDs Blue returns.
> ⚠️ `canvas_version` is the **sha256 64-hex** layout hash, not a literal.
> Pinned in §1 (reproduced by the ZabCanvas test).

## 1. Shell setup

```bash
export GW="https://zabgate.cyell.dev"
export RULE_SCENE_ID="$(uuidgen)"      # the stream-rule scene UUID — record it
export REACTIVE_SCENE_ID="$(uuidgen)"  # the active reactive scene UUID — record it
# OP_TOKEN: operator/admin bearer, exported in the operator shell ONLY.
# Never paste it into this file, the incident log, or a command echo.

# The finale Canvas layout content hash (pinned; reproduced by
# ZabCanvas/tests/test_quasar_finale_reactive_scene_layout.py).
export CANVAS_VERSION="7484582c1ac879525424feb0a2683cf6181c83af22c3a071317caf265814ed30"
```

Prerequisite (already deployed): Quasar is connected to `g2nmathias`
(IRC live) and its service-token WS to Orion is up, with
`X-Authenticated-Paths` including `platform.twitch.*` (the M9 gate). A
service-role writer staying connected without an active scene is expected.

Payloads via **stdin (`-d @-`)** where shown — `jq` is absent on the
Windows host; the `jq -n` forms below are the operator-shell variant.

## 2. Author the two blueprints in Blue

For EACH blueprint: create → write graph onto draft v1 → publish v1.

### 2.1 Stream-level rule — `bp-quasar-finale-stream-rule`

```bash
curl -fsS -X POST "$GW/blue/api/v1/blueprints" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d '{"slug":"bp-quasar-finale-stream-rule",
       "name":"Quasar finale stream-level rule","kind":"workflow",
       "tags":["adr013","stream-rule","twitch","platform-event"]}'
# 201 → record "id" as RULE_BP_UUID.
export RULE_BP_UUID="<id from the 201 response>"

# Write graph (quasar-finale-stream-rule-bp-graph.json lives in the Blue repo).
curl -fsS -X PUT "$GW/blue/api/v1/blueprints/$RULE_BP_UUID/versions/1" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "$(jq -n --argjson g "$(cat quasar-finale-stream-rule-bp-graph.json)" '{graph:$g}')"
# 200 → draft v1 carries the 28-node / 28-edge rule (14 on-platform-event spines).

curl -fsS -X POST "$GW/blue/api/v1/blueprints/$RULE_BP_UUID/versions/1/publish" \
  -H "authorization: Bearer $OP_TOKEN"
# 200 → current_version == 1.
```

### 2.2 Reactive scene — `bp-quasar-finale-reactive-scene`

```bash
curl -fsS -X POST "$GW/blue/api/v1/blueprints" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d '{"slug":"bp-quasar-finale-reactive-scene",
       "name":"Quasar finale reactive scene","kind":"workflow",
       "tags":["adr013","reactive","twitch","chat","dataflow"]}'
# 201 → record "id" as REACTIVE_BP_UUID.
export REACTIVE_BP_UUID="<id from the 201 response>"

curl -fsS -X PUT "$GW/blue/api/v1/blueprints/$REACTIVE_BP_UUID/versions/1" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "$(jq -n --argjson g "$(cat quasar-finale-reactive-scene-bp-graph.json)" '{graph:$g}')"
# 200 → draft v1 carries the pure-dataflow display chain.

curl -fsS -X POST "$GW/blue/api/v1/blueprints/$REACTIVE_BP_UUID/versions/1/publish" \
  -H "authorization: Bearer $OP_TOKEN"
# 200 → current_version == 1.
```

> The editor validator may return advisory port-name findings —
> **EXPECTED and NON-FATAL** (same as the canary/M1): Blue returns
> findings, never raises; Orion partitions the RAW graph on its own ports.

## 3. Store the Canvas layout (content-addressed)

The reactive scene renders `chat.display` via a single text element. Store
the bundle so `GET /canvas/api/v1/layouts/{hash}` (Orion compile step 1)
resolves it. **Both scenes push with this same `$CANVAS_VERSION`** — the
rule never goes on air, so its rendered layout is immaterial; reusing the
one stored bundle keeps the push valid without a throwaway layout.

```bash
# From the ZabCanvas repo root, regenerate the exact bundle the test pins:
python - <<'PY' > /tmp/finale-layout-bundle.json
import json, sys
sys.path.insert(0, "tests")
from test_quasar_finale_reactive_scene_layout import _finale_reactive_bundle
print(json.dumps(_finale_reactive_bundle()))
PY
# Its scene_version is sha256:7484582c…ed30 (== $CANVAS_VERSION).

BUNDLE="$(cat /tmp/finale-layout-bundle.json)"
ARCHIVE_B64="$(printf '%s' "$BUNDLE" | base64 -w0)"
curl -fsS -X PUT "$GW/canvas/api/v1/lsml-bundles/$CANVAS_VERSION" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "$(jq -n --argjson b "$BUNDLE" --arg a "$ARCHIVE_B64" '{bundle:$b,archive:$a}')"
# 201/200. Confirm Orion's compile step 1 resolves the layout:
curl -fsS "$GW/canvas/api/v1/layouts/$CANVAS_VERSION" \
  -H "authorization: Bearer $OP_TOKEN" | jq '.root.children[0].bindings.text'  # "chat.display"
```

## 4. Push both scenes (Orion)

Legacy single-blueprint envelope: Blue **UUID** as `blue_blueprint_id`,
Canvas **hash** as `canvas_version`.

```bash
# 4a. Reactive scene.
curl -fsS -X POST "$GW/orion/api/v1/scenes/$REACTIVE_SCENE_ID/push" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"canvas_version\":\"$CANVAS_VERSION\",\"blue_blueprint_id\":\"$REACTIVE_BP_UUID\"}"
# 200 + {"scene_version":"sha256:…","diagnostics":{"errors":[],...}}.
export REACTIVE_SCENE_VERSION="<scene_version from the response>"

# 4b. Stream-level rule scene.
curl -fsS -X POST "$GW/orion/api/v1/scenes/$RULE_SCENE_ID/push" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"canvas_version\":\"$CANVAS_VERSION\",\"blue_blueprint_id\":\"$RULE_BP_UUID\"}"
# 200. The compiler maps each core.event.on-platform-event@1 → an
# ExecEntry{Kind:"on-platform-event"} and synthesizes platformStreamBindings
# over the 14 last_<type> leaves on g2nmathias (ADR 013 RC #1).
export RULE_SCENE_VERSION="<scene_version from the response>"
# 422 COMPILE_FAILED → bp unpublished / wrong UUID / Blue unreachable.
# 422 with EXEC_OP_UNMAPPED → Orion predates #170 (see §0 precondition).
# 422 FETCH_UPSTREAM "Layout not found" → CANVAS_VERSION not stored (run §3).
# PLATFORM_CHANNEL_INVALID → a channel handle is malformed (must be g2nmathias).
```

## 5. Validate BOTH (R9) — exec runs only on a validated scene

The rule carries exec logic (`on-platform-event` → `show.emit`), so it
must clear the R9 validation bar before it can be promoted (ADR 009 §3
"exec gated R9"). The reactive scene is pure dataflow (no exec), but it
still runs through the same validate-then-activate gate (a scene must be
validated before it can go on air — `SCENE_NOT_VALIDATED`), so validate
both.

```bash
# Validate (202 Accepted), then poll until validated — for each scene.
for pair in "$REACTIVE_SCENE_ID:$REACTIVE_SCENE_VERSION" "$RULE_SCENE_ID:$RULE_SCENE_VERSION"; do
  SID="${pair%%:*}"; SV="${pair##*:}"
  curl -fsS -X POST "$GW/orion/api/v1/scenes/$SID/validate" \
    -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" -d '{}'
  until curl -fsS "$GW/orion/api/v1/scenes/$SID/validation?v=$SV" \
    -H "authorization: Bearer $OP_TOKEN" | jq -e '.status == "validated"' >/dev/null
  do sleep 1; done
  echo "$SID validated"
done
```

## 6. Rollback prep (pick the fallback BEFORE going on air)

```bash
curl -fsS "$GW/orion/api/v1/show" -H "authorization: Bearer $OP_TOKEN" | jq .
export ROLLBACK_ID="<uuid of a healthy, validated scene>"
```

## 7. Activate the reactive scene — it goes on air

```bash
curl -fsS -X POST "$GW/orion/api/v1/show/active-scene" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"scene_id\":\"$REACTIVE_SCENE_ID\"}"
# 200. The scene is the antenna; its core.input(__events.stream_chat_event)
# reads the rule's emit reactively. (Activate-before-validate is refused
# 409 SCENE_NOT_VALIDATED — §5 satisfies the gate.)
```

## 8. Promote the stream-level rule — it becomes always-on

The rule is a SCOPE, not the antenna: promote it (do NOT activate it). It
must be a *different* scene from the active one (a rule and the antenna
are disjoint roles — `RULE_IS_ACTIVE_SCENE` otherwise).

```bash
curl -fsS -X POST "$GW/orion/api/v1/show/stream-rules" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"scene_id\":\"$RULE_SCENE_ID\"}"
# 200 + {"stream_rule_id":"<RULE_SCENE_ID>"}. The rule now runs in the
# union {active} ∪ {stream-rules} (ADR 009 §3.3) and is reseeded at boot.
# 409 SCENE_NOT_VALIDATED → §5 not completed for the rule.
# 409 SCENE_NOT_PUSHED   → §4b not completed.
# 409 RULE_IS_ACTIVE_SCENE → you passed the reactive scene id; pass RULE_SCENE_ID.

# Confirm promotion. NOTE: the deployed `GET /show` summary does NOT echo a
# `stream_rules` field (it is omitted when the serializer has none). The
# authoritative signals are (a) the 200 response above carrying
# {"stream_rule_id": "<RULE_SCENE_ID>"}, and (b) the persisted row:
#   docker exec orion-postgres psql -U orion -d orion -tAc \
#     "SELECT scene_id FROM show_stream_rules ORDER BY promoted_at;"
# `GET /show/stream-rules` is 405 (only POST/DELETE on that path).
curl -fsS "$GW/orion/api/v1/show" -H "authorization: Bearer $OP_TOKEN"
```

## 9. Go-live Solar (browser source)

Per `live-testing.md` §4: point Pulsar's browser source at Solar v0.2.9+
with the show-token WS, then StartDestination RTMP Twitch. The text
element bound to `chat.display` is on screen (empty until the first chat).

## 10. The franchissement (porteur triggers; the proof)

> **The porteur (or a second account) sends a message in the `g2nmathias`
> Twitch chat.** The bot cannot self-test — Twitch IRC does not echo the
> sender's own PRIVMSG to its own session (`live-testing.md` §5).

The single load-bearing proof is the antenna, recorded to `.mp4`
(`live-testing.md` "Règle d'or" — a wire snapshot is insufficient):

| Signal | Where | Healthy value |
|---|---|---|
| Antenna text | recorded `.mp4` (ffprobe/ffmpeg) | the chat text appears/updates on the `chat.display` element on each new message |
| `chat.display` | active scene live snapshot | the last chat message's text — changes reactively, no re-push |

That string changing at the antenna on a real chat message is the finale:
`quasar.twitch.chat@1` write → `on-platform-event` arms → `show.emit`
injects `__events.stream_chat_event` into the active scene → its
`core.input` read wakes the get-field → output chain → `chat.display`
repaints. The 14-type coverage means follow/sub/raid/cheer/…
ride the same proven path.

> If nothing changes at air: confirm (a) Quasar's WS to Orion is up and
> `X-Authenticated-Paths` includes `platform.twitch.*`; (b) the channel is
> `g2nmathias` byte-for-byte; (c) the reactive scene is the ACTIVE one and
> the rule is PROMOTED (`/show` shows both); (d) Solar is v0.2.9+; (e) the
> rule and reactive scene are distinct scene ids.

## 11. Rollback

```bash
# Off-air the reactive scene (single active-scene switch, no recompile):
curl -fsS -X POST "$GW/orion/api/v1/show/active-scene" \
  -H "authorization: Bearer $OP_TOKEN" -H "content-type: application/json" \
  -d "{\"scene_id\":\"$ROLLBACK_ID\"}"

# Demote the rule (idempotent; cancels its live tasks):
curl -fsS -X DELETE "$GW/orion/api/v1/show/stream-rules/$RULE_SCENE_ID" \
  -H "authorization: Bearer $OP_TOKEN"
```

Both scenes stay pushed/validated for post-mortem; they are simply off
air / out of the rule set.

## 12. Post-flight

- Record `$RULE_SCENE_ID`, `$REACTIVE_SCENE_ID`, both `$*_BP_UUID`,
  `$CANVAS_VERSION`, both `$*_SCENE_VERSION`, the observed antenna result,
  and whether rollback was used (never the token).
- Scrub the show-token JWT (`eyJ…`) from the CEF logs after the test
  (`live-testing.md` security note).
- The blueprints (published v1) and scenes may stay as the standing finale
  artefacts, or be archived per Keeper's call.
