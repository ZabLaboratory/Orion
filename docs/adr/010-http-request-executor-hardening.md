# ADR 010 — `core.http.request@1` canonical executor + content hardening

- **Status**: accepted
- **Date**: 2026-06-13
- **Decided**: 2026-06-13
- **Deciders**: @ClodoCapeo
- **Author**: Forge (implementation) · Bastion (§3.7 content hardenings)
- **Supersedes**: the stub `execHTTPRequest` from ADR 003 §3.1.3 phase-3
  (PR #97 / `ce576d2`) — that stub wired the exec pins but forwarded neither
  `query`, `headers`, nor `response_headers`; it read `timeout_seconds` (a
  phantom field) rather than `timeout_ms`.
- **Superseded by**: —

> **Numbering.** Per repo convention (ADR 006 header note): 004/005 burned by
> in-tree citations; 007–009 committed. First free number after 009: **010**.

---

## 1. Context

Blue PR #76 gave `core.http.request@1` its exec pins (`in` / `then` / `error`),
making it a reachable world-effect for the first time. The Orion executor
(`exec_effects.go::execHTTPRequest`) pre-dated that seed change and was
incomplete: it did not forward authored `query` params or `headers`; it did not
bind the `response_headers` output pin; and it read `timeout_seconds` (a field
that does not exist in the seed) rather than `timeout_ms`. The Bastion clearance
(ADR 003 §3.7, phase-3 EgressPolicy) covered the **transport layer** (allowlist
fail-closed, post-DNS SSRF vetting) but not the **content layer** — three
hardening gaps remained open at the executor level.

---

## 2. Decision

### 2.1 Executor completion (the correctness half)

The `execHTTPRequest` executor is completed to match the current seed signature:

- **`query`** (map of string→string, authored config): merged into the
  request query-string on top of the authored `url`'s own query params. Both
  the authored URL query and the `query` map are forwarded; duplicates use
  standard `url.Values` semantics (last-write wins per key).
- **`headers`** (map of string→string, authored config): forwarded as
  outbound HTTP request headers, subject to the strip list (§2.2a).
- **`response_headers`** (output pin): the upstream response headers are
  bound to this output pin on a successful response, as a
  `map[string]string` (first value per header name, canonical casing).
- **`timeout_ms`** (authored config, integer): replaces the phantom
  `timeout_seconds`. Hard-clamped to a ceiling (§2.2b). Falls back to a
  default if absent or zero.

### 2.2 Content hardenings (the Bastion §3.7 gap-close)

Three hardenings applied on top of the unchanged EgressPolicy transport:

#### (a) Sensitive / hop-by-hop header strip

The following authored headers are dropped **case-insensitively** before the
outbound request is dispatched — Orion never forwards them regardless of what
a blueprint authors:

`Authorization`, `Cookie`, `Proxy-*` (prefix), `Host`, `Content-Length`,
`Connection`, `Transfer-Encoding`, `Upgrade`, `TE`, `Trailer`.

Rationale: Orion never has a legitimate need to forward `Authorization` (no
scene credential model — the approved asymmetry with Blue); forwarding session
cookies or hop-by-hop headers creates request-smuggling / SSRF escalation
vectors. The authored `url` is always an allowlisted host (EgressPolicy,
pre-existing, unchanged); stripping these headers is defence-in-depth against
a blueprint author that misuses the header map.

#### (b) Size caps and timeout clamp

- Cumulative outbound headers size: capped.
- Query-string length: capped.
- Request body size (if applicable): capped.
- `timeout_ms`: clamped to a hard ceiling (server-side maximum); a value above
  the ceiling is silently reduced to the ceiling. A zero or absent value falls
  back to the server default.

Exact cap values are constants in `exec_effects.go` (not duplicated here —
the code is the source of truth for numeric limits).

#### (c) Host-only error logging and error pin binding

On any transport failure (DNS error, connection refused, TLS failure, timeout,
SSRF block) `net/http` returns a `*url.Error` whose `.Error()` string re-echoes
the full request URL **including authored query parameters**. An authored API key
in `query` would leak into:
- the `error` output pin (bound to the blueprint graph),
- the `"effect failed"` log line (sent to the operator WS stream).

Fix: a `httpFailureReason(host, err)` helper extracts only the **host** (from
`*url.Error.URL`) and the unwrapped cause string, and never the path, query, or
`?` character. All four failure sites — `HTTP_REQUEST_FAILED`,
`HTTP_REQUEST_INVALID`, `HTTP_REQUEST_INVALID_URL`, and `egressDenied` — are
routed through this helper. `HTTP_REQUEST_INVALID_URL` emits a generic class
(a URL that failed `url.Parse` has no trustable host to extract).

> The SSRF / cloud-metadata dial-time case (`egressDenied` via EgressPolicy
> transport) was the highest-value attacker path: EgressPolicy raises
> `ErrEgressBlocked` inside the transport; `net/http` wraps it into a
> `*url.Error` containing the full URL. Without the fix, a blueprint targeting
> `169.254.169.254` with a secret in the query would have leaked the secret +
> path in the `error` pin despite the SSRF block. Fixed — the bound error
> carries only the host and `EGRESS_BLOCKED`.

---

## 3. Consequences

- `core.http.request@1` is now a **fully operational** world-effect exec op
  (was a reachable-but-incomplete stub post-Blue #76).
- Blueprints can author `query`, `headers`, and consume `response_headers` —
  the full seed surface is exercised.
- The `timeout_seconds` field is retired (it was never in the seed; the
  executor silently ignored it). Blueprints that authored it see no breakage
  (the field is unknown and ignored at compile; `timeout_ms` is the canonical
  name).
- Authored `Authorization`/`Cookie` headers are silently stripped (not an
  error). The strip is documented as doctrine, not a silent failure.
- Error strings on the `error` pin contain only `host: cause` — no URLs, no
  query secrets, no paths. Blueprint authors that pattern-matched on the raw
  URL string in the error pin will see a changed format (this was never a
  supported contract; the error pin shape was underspecified).
- `signatures.json` and `manifest.json` are regenerated: `core.http.request@1`
  now declares exec pins `in`/`then`/`error` (category: `side-effect`). Node
  count unchanged (82).

---

## 4. Security

This ADR closes the content-hardening gap left open by the Bastion phase-3
clearance (ADR 003 §3.7). The transport layer (EgressPolicy allowlist +
post-DNS SSRF vetting) is unchanged and remains the primary control; the
content layer (header strip, size caps, host-only logging) is defence-in-depth.

**No new egress surface is opened.** The EgressPolicy allowlist remains the
authoritative gate for which hosts `core.http.request@1` may reach — authoring
a request to an off-allowlist host fails closed.

**Bastion clearance** for the transport layer was granted in ADR 003 §3.7.
The content hardenings of this ADR are additive restrictions; they do not
require a fresh Bastion spawn (they reduce, not increase, the attack surface).

The leaguepedia public surface (`GET /truth/api/v1/leaguepedia/*` via ZabGate,
documented in `architecture.md`) is the primary intended target of
`core.http.request@1` in blueprints today. That route is allowlisted, GET-only,
and auth-stripped at ZabGate by PR #160's `Authorization`-drop — the two
controls are consistent.

---

## 5. Resolution criteria (implemented, verified by tests in PR #160)

1. `then` fires on a 2xx response; `error` fires on non-2xx or transport
   failure.
2. Authored `query` params appear in the outbound URL query-string.
3. Authored `headers` (excluding the strip list) are forwarded; strip-list
   entries (`Authorization`, `Cookie`, etc.) are silently dropped — asserted
   by a test that authors those headers and confirms they never reach the
   mock upstream.
4. `response_headers` output pin is bound to the upstream response headers
   on success.
5. `timeout_ms` governs the request timeout; a value above the ceiling is
   silently clamped.
6. **Error pin / log host-only**: a transport failure toward an
   allowlisted-but-dead host binds only `host: cause` to the `error` pin —
   the authored query (including any secret) never appears. Asserted by a
   network-failure test against an allowlisted-but-unreachable endpoint.
7. **SSRF / dial-time egressDenied host-only**: a request to an allowlisted
   host that resolves to a cloud-metadata IP (`169.254.169.254`) is blocked
   by EgressPolicy; the `error` pin carries only `host + EGRESS_BLOCKED` —
   never the secret, query param name, path, or `?`.
8. EgressPolicy allowlist remains unchanged; no off-allowlist host is
   reachable (existing test suite, unmodified).
