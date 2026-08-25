# Changelog

All notable changes to Orion land here. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Every release section is written *before* the tag is pushed — the
`release.yml` workflow extracts the section matching the tag and uses
it as the GitHub Release body. If a section is missing, the release
publishes with empty notes.

## [2.0.0] - 2026-08-25

### Changed

- Publish the embedded-local Orion sidecar contract as Orion 2.0.0.
- Keep the Blue runtime digest and source revision bound to the sidecar marker.


### Release diff summary

- Range: `v1.1.0..v2.0.0`.
- Complete ancestry: 302 commits.
- Diffstat: 437 files changed, 116237 insertions(+), 4893 deletions(-)

### Complete commit inventory

- `9b841fd` 2026-06-10 - docs(adr): ADR 003 — dataflow-only runtime + platform-event ingestion (accepted) (#75)
- `e9de827` 2026-06-10 - revert(adr): remove ADR 003 — rejected by product owner (#77)
- `8d63e94` 2026-06-10 - docs(adr): ADR 003 — full Blue execution engine + scene-validation gate + platform ingestion (#78)
- `d3ffacf` 2026-06-10 - feat(compiler,runtime): carry named ports into the graph artefact (#90)
- `7344f64` 2026-06-10 - perf(runtime): O(1) graph indexes, dirty-cone recompute, 20k CI gate (#80) (#91)
- `19e795e` 2026-06-10 - feat(runtime,compiler): pure data-node registry tranche — 27 executors (ADR 003 phase 0) (#92)
- `0745573` 2026-06-10 - feat(runtime): exec interpreter with explicit continuations (ADR 003 phase 1) (#93)
- `f68a6e4` 2026-06-10 - feat(runtime): timer wheel, delay, triggers, cancellation — ADR 003 phase 1 (#83) (#94)
- `5aad04d` 2026-06-10 - feat: platform leaf binding + platform-stream acceptance + inbox drop metric (ADR 003 phase 2) (#95)
- `a3d6fc0` 2026-06-10 - docs(adr): amend ADR 003 §3.1.3 db.query topology A + control-mode scope + R9 residual risk (Refs #85 #86) (#96)
- `ce576d2` 2026-06-10 - feat(runtime): async effect executor — http.request, db.query, source.read (ADR 003 phase 3, #85) (#97)
- `ff2a053` 2026-06-10 - feat(runtime,api): animation.play + external completion endpoint (ADR 003 phase 3, #86) (#98)
- `5697046` 2026-06-10 - feat: scene-validation gate (ADR 003 phase 4, #87) (#99)
- `7fc738a` 2026-06-10 - feat(obs): per-scene-version exec CPU counter (B7) (#101)
- `da49c70` 2026-06-10 - ci(prod): cap orion container CPU at 1 core (ADR 003 R7/B7 isolation, #89) (#100)
- `c145e49` 2026-06-10 - feat(conformance): total-conformance matrix CI gate (ADR 003 phase 5, #88) (#102)
- `1ae92e2` 2026-06-10 - docs(adr): ADR 006 (proposed) — exec activation: compiler partition + R9 lift (#110)
- `5080b2b` 2026-06-10 - refactor(runtime): make B10 validation-mode guard introspective (#108 P-1) (#112)
- `ac9f343` 2026-06-10 - ci(prod): recalibrate orion cpus cap post-nproc + add exec canary runbook (#111)
- `bd06b38` 2026-06-10 - docs(adr): correct stale B1 ledger + inscribe accepted residuals (R-SR/jti/matchPath) + resync criteria (#113)
- `50196bb` 2026-06-11 - feat(compiler): partition exec layer, emit ExecPrograms (#103, R9 fondation) (#115)
- `71b722c` 2026-06-11 - [#107] conformance: reclassify 6 core.db.* atoms inline-only; empty allowlist (#114)
- `8385428` 2026-06-11 - feat(runtime): install the full exec-program set per scene (R9 multi-program, #105) (#116)
- `c1358c6` 2026-06-11 - feat(api,runtime): R9 lift — exec to air behind the validation gate (#106) (#117)
- `7ec2a5a` 2026-06-11 - docs(runbook): revalidate exec first-flight canary against #106 reality (#118)
- `d0c9c3c` 2026-06-11 - docs: resync ADR 006 §5 halt-at-node, drop dead _shared imports, commit phase-3 contract (#119)
- `e2690ff` 2026-06-11 - ci: bump GitHub actions to Node24-compatible majors (#120)
- `4074494` 2026-06-11 - docs: re-import _shared transverse docs in Orion CLAUDE.md (#121)
- `288ab61` 2026-06-11 - docs(runbook): canary first-flight scene payload + commands (Refs #106) (#122)
- `f88311a` 2026-06-11 - docs(runbook): post-vol canary R9 — verdict, observables, deux pièges corrigés (#123)
- `5060773` 2026-06-11 - fix(ws): service writers hold /show/stream without an active scene (#124)
- `e32fb09` 2026-06-11 - test(e2e): prove reactive Twitch chat dataflow end-to-end (M1) (#125)
- `efe23b0` 2026-06-11 - feat(runtime): wire async effects on air behind the validation gate (R9 lift) (#126)
- `edd9ac1` 2026-06-11 - ci(deploy): make db.query config durable across .env rewrites (#127)
- `a3fc2c6` 2026-06-11 - test(m2): e2e proof + runbook for the live-data scene (db.query + chat) (#128)
- `5efa229` 2026-06-11 - test(m3): named leaderboard via static unroll (5 sequential truth queries) (#129)
- `a41f922` 2026-06-11 - fix(m3): assert leaderboard maps summoner_name not nullable display_name (#130)
- `89f67ac` 2026-06-11 - test(m3): reactive named board — twins + prod-graph regression guard (#131)
- `200b710` 2026-06-11 - fix(lsdp): emit only scalar leaves on the wire — §3.2.1 conformance (black-screen root cause) (#132)
- `93e133a` 2026-06-12 - fix(lsdp): emit only the bundle's bound-leaf surface on the wire (#133)
- `5ba4816` 2026-06-12 - fix(show): re-activate the persisted active scene on boot (#134)
- `0b6c190` 2026-06-12 - fix(compiler): lower text content key `text` to render vocab `value` (#136)
- `03f4307` 2026-06-12 - test(runtime): prove M4 chat-driven draft switch (data changes A→B) (#137)
- `b56b981` 2026-06-12 - test(m4): e2e draft switch over 3 pinned real drafts (#138)
- `590684c` 2026-06-12 - feat(db): core.db.* atomics as pure descriptor builders + 7/7 conformance (ADR 007) (#142)
- `10e5ef1` 2026-06-12 - fix(compiler): bind core.variable.get@1 to the __vars leaf set writes (#144)
- `08a1ff1` 2026-06-12 - ci(e2e): isolate each requireDB in an ephemeral schema (fix 23505 on shared DB) (#145)
- `747ffad` 2026-06-12 - docs(runbook): e2e shared-DB schema isolation hotfix (PR #145)
- `8afc1ff` 2026-06-12 - fix(contract): align runtime to seed port/config names + from-table threading + parity gate (#146)
- `80ef7ba` 2026-06-12 - fix(runtime): iterate counted-loop body with bound index/element (#147)
- `186db05` 2026-06-13 - feat(runtime): active-scene-only execution (ADR 008) (#152)
- `490d233` 2026-06-13 - feat(runtime): stream-rule union routing + non-frozen lifecycle (ADR 009, #153) (#156)
- `3858e0e` 2026-06-13 - feat: stream-rule promotion API + core.show.emit@1 executor (ADR 009, #154/#155) (#157)
- `d52e55b` 2026-06-13 - feat(runtime): complete core.http.request@1 executor + ADR 010 hardening (#158) (#160)
- `e5d439b` 2026-06-13 - fix(runtime): re-activation refires on-start even when already active (#163)
- `9c91f27` 2026-06-13 - feat: animation.play keyframe lowering + scalar generation leaf (ADR 011 I2/I3/I4) (#161)
- `7fa7c26` 2026-06-13 - docs(contract): reconcile __anim wire shape obj→scalar + wake-key decision (ADR 011 I5) (#164)
- `2378f4a` 2026-06-13 - docs(contracts): fix source.read compute bundle contract (ADR 012 Option B) (#165)
- `eaa096e` 2026-06-13 - fix(compiler): animation.play wraps + nests target overlay (ADR 011 I7) (#167)
- `266f79f` 2026-06-13 - feat(compute): reclassify core.source.read@1 to a pure compute (ADR 012 Option B) (#166)
- `8b1174e` 2026-06-13 - ci(deploy): reconcile SOLAR_VERSIONS, add v0.2.9 bundle (#168)
- `1dfc0da` 2026-06-13 - docs(resync): ADR 010/012, Solar v0.2.9 runbook, CLAUDE.md ledger (#169)
- `0aa91d0` 2026-06-13 - feat(exec): add core.event.on-platform-event@1 arming entrypoint (#170)
- `f0d20a0` 2026-06-13 - docs(runbook): quasar finale stream-level rule + reactive scene (ADR 013) (#171)
- `03523d5` 2026-06-13 - docs(runbook): correct finale reactive scene to dataflow-only + /show note (#172)
- `f0c3e7d` 2026-06-13 - fix(finale): reactive dataflow output + on-platform-event payload binding (ADR 013) (#173)
- `06cee6e` 2026-06-13 - fix(runtime): bind on-event payload into the fired task env (#174)
- `838f6ad` 2026-06-13 - test(runtime): prove __events leaf drives reactive dataflow (ADR 013 #4) (#175)
- `f51ca10` 2026-06-13 - fix(compiler): event-topic acceptance binding for __events.* dataflow inputs (finale 5th link) (#176)
- `621189a` 2026-06-13 - fix(runtime): value-equality for core.compare.equal/not-equal@1 (#177)
- `ca467d4` 2026-06-14 - docs(adr): ADR 014 — blueprint-reference compile-time expansion (accepted) (#182)
- `3a2e0ae` 2026-06-14 - feat(compiler): expand blueprint-reference nodes at compile time (ADR 014) (#183)
- `308a174` 2026-06-14 - feat(compiler): detect cyclic blueprint references at compile (ADR 014) (#184)
- `596dfe3` 2026-06-14 - test(compiler): expand-reference matrix #180 — ADR 014 RC coverage (#185)
- `2958616` 2026-06-14 - feat(compiler): map exec pins + drop on-start in reference expansion (#187)
- `1258493` 2026-06-14 - fix(compiler): splice exec edges between adjacent expanded references (#189)
- `348bb54` 2026-06-14 - fix(compiler): promote __vars input readers in reference expansion (ADR 014) (#190)
- `36658aa` 2026-06-14 - fix(compiler): reject dangling exec targets at compile (EXEC_UNKNOWN_NODE) (#188)
- `c969333` 2026-06-14 - docs(runbook): add scene recompile/repush procedure after compiler fix (#191)
- `443fe84` 2026-06-14 - fix(compiler): seed referenced functions' variables[].value into graph defaults (#193)
- `128f856` 2026-06-16 - feat(validate): service-scoped simulate endpoint (ADR 015) (#198)
- `8332d19` 2026-06-17 - fix(validate): compile Blue graph in-body for simulate (#200)
- `f42b469` 2026-06-17 - fix(simulate): reject zero-program / unknown-unwired graphs loudly (#201)
- `5a008d9` 2026-06-18 - feat(compiler): LSML emit 1.2 + preserve assets.allowedHosts (ADR 002 #G, T6) (#202)
- `39f21bf` 2026-06-18 - test(compiler): pin id + shape-mask ref round-trip through emit_lsml (ADR 002 #K) (#203)
- `8c6cbcb` 2026-06-18 - feat(gate): refuse hostile/over-budget LSML bundles before the antenna (#204)
- `e671fd7` 2026-06-18 - test(compiler): assert group-source mask round-trips through emit (#O) (#205)
- `fac8362` 2026-06-20 - ci: add hard-fail govulncheck job (#207)
- `16b656b` 2026-06-20 - test(runtime): prove chained-reference db.query exec path reaches ref2 (#192)
- `2555ed3` 2026-06-20 - docs(orion): resync CLAUDE.md — status prod, endpoints complets, ADR 013-015 (#206)
- `956e5b3` 2026-06-20 - ci: route Orion to shared org-scoped VPS runner pool (#208)
- `7a93c39` 2026-06-21 - feat(api): introspectable DB catalog — GET /db/{service}/schema + db.table datasources (#211) (#212)
- `db4fe7a` 2026-06-21 - feat(runtime): operator on-call + await-value runtime surface (#209) (#213)
- `2f6f47b` 2026-06-21 - feat(api): derive GET /cockpit/contracts operator UI aggregate (#215)
- `03423b9` 2026-06-21 - docs(runbook+claude): PG e2e idempotent bootstrap + Blue ADR 008 resync (#216)
- `b37b66b` 2026-06-21 - docs(runbook): local execution stack — Orion engine end-to-end, no antenna (#217)
- `e43db20` 2026-06-21 - docs(adr): ADR 016 embedded-local-execution-profile (accepted) (#227)
- `f4ea144` 2026-06-21 - feat(boot): Store + AuthSource interfaces + ORION_PROFILE flag (ADR 016 foundations) (#229)
- `6290d3e` 2026-06-21 - docs(contracts): freeze embedded-local _query + bundle contracts (#228)
- `9c00f70` 2026-06-21 - feat(embedded-local): sqliteStore + bundledFetcher (ADR 016 §3.2) (#232)
- `4ddc3dc` 2026-06-21 - feat(datasidecar): embedded-local _query data sidecar with SQLite mirrors (#231)
- `a3bfb0c` 2026-06-21 - feat(auth): localOperatorAuth loopback shim (embedded-local) — #223 (#230)
- `316ea9a` 2026-06-21 - ci(e2e): make Postgres setup idempotent + privilege-aware (fix apt flake) (#233)
- `09f2fba` 2026-06-21 - docs(runbook): CI runner image with pre-baked PostgreSQL + sudo (#234)
- `c956613` 2026-06-21 - docs(contracts): amend §B — frozen graph nodes must carry baked ports (#236)
- `ea14493` 2026-06-21 - test(e2e): embedded-local on-call LCK/LEC proof harness (ADR 016 RC-6) (#235)
- `eefd3b3` 2026-06-21 - fix(compiler): fold top-level blueprint variable constants at compile (#237)
- `0978569` 2026-06-21 - feat(operator): address legacy default-blueprint on-call via "_" token (#238)
- `d1ebe04` 2026-06-21 - ci: disable setup-go cache on the self-hosted pool (fix 1.1 GB hang) (#239)
- `af5e608` 2026-06-21 - test(e2e): Phase A franchie — 40/40 leaves via HTTP route, LCK + LEC (#240)
- `4ede646` 2026-06-21 - ci: add win-sidecar workflow to cross-compile Windows sidecar binaries (#242)
- `c9ba95b` 2026-06-22 - fix(ws): derive show WS identity through configured AuthSource (#243)
- `1ba4626` 2026-06-22 - fix(lsdp): derive LSDP wire identity through configured AuthSource (#244)
- `3d1c62e` 2026-06-22 - docs(contracts): add Contract C — /canvas + /blue loopback (ADR 016 A1) (#248)
- `292205a` 2026-06-22 - feat(profile): wire httpFetcher in embedded-local for scene-agnostic boot (#249)
- `4bee41b` 2026-06-22 - docs(adr): ADR 016 Amendment 1 — embedded-local scene-agnostic (accepted) (#250)
- `20e93c7` 2026-06-22 - docs(adr): ADR 016 Amendment 2 — two-process loopback mirror topology (accepted) (#251)
- `c6e80d2` 2026-06-22 - feat(profile): import air-eligibility from validation mirror in embedded-local (#252)
- `2be821f` 2026-06-22 - docs(adr): ADR 016 RC-A5 — précise l'import-verdict de validation locale (impl #247) (#253)
- `694fcbb` 2026-06-22 - fix(profile): key embedded-local validated mirror by canvas_version + snake_case (#254)
- `ae410a9` 2026-06-22 - fix(embedded-local): faithful preview — layout defaults, blueprint variables, preview host-allow (#255)
- `9aa1fd6` 2026-06-23 - feat(show): preview→air state hand-off seams (#256) (#257)
- `0ee3818` 2026-06-24 - ci(deploy): pin Solar v0.2.10+v0.2.11 + runbook for manual capture-fix deploy (#258)
- `4e50a62` 2026-06-28 - feat(lsdp): per-session preview LSDP source (preview ≠ antenne) (#259)
- `c333690` 2026-06-28 - feat(egress): service.call handler + curated route compile reject (#264)
- `8de4b7c` 2026-06-28 - feat(egress): per-stream budget/rate-limit for core.service.call (R3) (#266)
- `2be7f24` 2026-06-28 - feat(runtime): assign-slot handler + stream-level mirror + LSDP delta (#267)
- `df1294b` 2026-06-28 - feat(lsdp): carry Meet viewer credentials on the antenne wire (#261) (#268)
- `315f237` 2026-06-28 - test(lsdp): end-to-end meet-cam pipeline contract (#262) (#270)
- `1055d6c` 2026-06-29 - fix(compiler): carry assets.allowedHosts onto the bespoke RenderBundle (#271)
- `920d040` 2026-06-30 - feat(stream-rules): register a stream-rule from a blueprint + list endpoint (#272)
- `d119983` 2026-07-02 - feat(lsdp): emit scene_roster preload frame from the Show (#275)
- `251ac21` 2026-07-02 - fix(switch): idempotent re-push, on-air swap guard, content-addressed fetch cache (#276)
- `b8d8f63` 2026-07-03 - docs(runbooks): document Playwright/Chromium libs bake on org-runner pool (#277)
- `9d60718` 2026-07-06 - fix(lsdp): parallelise viewer-creds resolution so all guests arm on air (#278)
- `4ad9c03` 2026-07-07 - fix(compiler): version-gate the layout cache key against ZabCanvas serialization changes (#279)
- `b4cfd66` 2026-07-07 - fix(compiler): bump layoutContractVersion to v3 for ZabCanvas #152 (#280)
- `837a389` 2026-07-09 - docs(runbook): CI dispatch dead — orchestrator backstop poisoned by 404 served-repo (#281)
- `34f3080` 2026-07-10 - feat(overlay): core.overlay-app.set@1 executor + stream-level overlay mirror (#283) (#284)
- `794b66b` 2026-07-10 - docs(adr): commit ADR 009 Amendment 1 — never pushed since acceptance (#288)
- `80546c7` 2026-07-10 - feat(cockpit): stamp rule_id on stream-scope contract items (#289)
- `e8e7dc4` 2026-07-10 - feat(operator): add ?rule={rule_id} selector to operator routes (#290)
- `76bcc74` 2026-07-10 - feat(stream-rules): persist + boot-reseed blueprint-direct rules (#291)
- `8f61577` 2026-07-10 - feat(lsdp): carry overlay-app control on the overlay_apps frame (#293)
- `b09d3f5` 2026-07-22 - fix(deps): bump golang.org/x/text to v0.39.0 (GO-2026-5970) (#295)
- `0afd956` 2026-07-22 - fix(operator): address on-call by config.entrypoint, blueprint-direct rules by id (#294)
- `17b622f` 2026-07-22 - docs(adr): ADR 017 — compile plein-graphe des stream-rules blueprint-direct (#296)
- `019a728` 2026-07-22 - feat(compiler): full-graph compile for blueprint-direct stream rules (#297)
- `503be96` 2026-08-05 - feat(push): surfacer LSML_HASH_MISMATCH dans la réponse 200 (#298)
- `e40844c` 2026-08-05 - feat(compiler): vérifier l'adresse d'un bundle LSML avant d'injecter ses defaults (#299)
- `01a6152` 2026-08-08 - feat(store): persister le refresh token de service chiffré (AES-256-GCM) (#308)
- `a5a8f24` 2026-08-08 - feat(auth): ServiceTokenManager durable — refresh au boot, persist-before-swap (#309)
- `f9b1c4c` 2026-08-08 - feat(auth): garde de profil + advisory lock Postgres pour le service token durable (#310)
- `0077ca3` 2026-08-08 - refactor(auth): retirer ORION_OPERATOR_TOKEN et supprimer EgressTokenSource (#311)
- `41a2a6f` 2026-08-08 - ci(deploy): exiger ORION_ENCRYPTION_KEY au preflight, retirer ORION_OPERATOR_TOKEN (#312)
- `4dde634` 2026-08-08 - fix(deploy): un seul démarrage d'Orion par deploy (#307) (#313)
- `28af2e4` 2026-08-08 - docs(runbook): consigner le déroulé réel du cutover service-token (#314)
- `21d9af9` 2026-08-09 - fix(compiler): split size into flat width/height for layout kinds (#315)
- `af78d5d` 2026-08-11 - docs: accept ADR 018 — Postgres CI natif sur le substrat runner Zab (#316)
- `622144c` 2026-08-11 - docs: propose Postgres CI canon for Zab conventions.md (#319) (#322)
- `936dbd2` 2026-08-11 - docs(runbooks): document PG16 runner image cutover (ADR 018 §3.2) (#323)
- `233f4ce` 2026-08-11 - ci: assert PostgreSQL major version 16 in e2e job (ADR 018 §3.2, R-7) (#324)
- `eec7bf5` 2026-08-11 - fix(compiler): adapt ZabCanvas pushed component contract
- `a3b6ff5` 2026-08-11 - Merge pull request #328 from ZabLaboratory/conduit/245-component-contract
- `06c7f3f` 2026-08-11 - fix(lsdp): reject anonymous viewer credential fetches
- `f1635f8` 2026-08-13 - spike: Blue runtime Go module consumable from Orion (#342)
- `d521986` 2026-08-13 - feat: stateless-cutover workload/attestation/bluehost stack (Phase A, #331) (#343)
- `59d7719` 2026-08-13 - feat(api,bluewire): §6.7 idempotence + explicit slot replacement policy (#344)
- `f5943ed` 2026-08-13 - feat(api,cmd)!: remove all legacy Store-backed code (final #15 cutover) (#346)
- `48127d9` 2026-08-13 - docs(runbooks): legacy-store deploy fix + orion credential revoke (#331) (#347)
- `1d001fa` 2026-08-13 - feat(providers): wire Zab capability providers into scene-intent Prepare/Take (#348)
- `4f8cc62` 2026-08-14 - fix(ci): purge stale Blue VCS cache before go mod download (#350)
- `25d9ba4` 2026-08-14 - docs(runbook): document Blue VCS cache poisoning fix (PR #350) (#351)
- `3409b27` 2026-08-14 - fix(ci): purge whole Go VCS cache, not remote-matched subset (#352)
- `948b2e4` 2026-08-14 - docs(runbook): update Blue VCS cache fix runbook with v2 (#353)
- `5cf6c69` 2026-08-14 - fix(ci): scope Blue-read git config to the job, not global HOME (#354)
- `d37cd7c` 2026-08-14 - fix(ci): pin Go toolchain to 1.26.6, closes 6 stdlib CVEs (#355)
- `69f9a93` 2026-08-14 - docs(runbook): v3/v4 (config race, toolchain pin) (#356)
- `3914dfa` 2026-08-14 - feat(bluehost): execute core.http.request effect invocations for real (#349)
- `e623b3f` 2026-08-14 - fix(conformance): vendor core.overlay-app.set@1 in manifest (#357)
- `abcf676` 2026-08-14 - feat(bluehost): wire Engine B host for tick/call/platform-event + http/db effects (#359)
- `31bafd3` 2026-08-14 - fix(bluehost): unify HTTP transport dependencies
- `c786a89` 2026-08-14 - fix(conformance): wire service.call across bluehost
- `c84d6f1` 2026-08-14 - fix(bluehost): remove obsolete numeric helper
- `14093bf` 2026-08-14 - fix(bluehost): match service call validation order
- `88ca3fa` 2026-08-14 - Merge pull request #360 from ZabLaboratory/forge/358-bluehost-effect-coherence
- `b30de5a` 2026-08-14 - fix(deploy): authenticate private Blue module build
- `6818c56` 2026-08-14 - Merge pull request #361 from ZabLaboratory/forge/318-deploy-blue-read
- `fccff08` 2026-08-14 - fix(deploy): install git for private module fetch
- `6e42743` 2026-08-14 - Merge pull request #362 from ZabLaboratory/forge/318-deploy-blue-read
- `0ce1694` 2026-08-14 - fix(deploy): pin production toolchain inputs
- `36f09ff` 2026-08-15 - [ENGINE-B-PARITY] Deliver stateless Orion host with strict A/B parity
- `7488435` 2026-08-15 - test(runtime): prove A/B behavior for all 87 primitives (#365)
- `228f86d` 2026-08-15 - feat: wire Orion software-P256 workload identity (#366)
- `3c60a01` 2026-08-15 - feat(deploy): automate Orion workload identity rotation (#367)
- `a87e16a` 2026-08-15 - fix(deploy): allow Orion identity agent socket (#368)
- `e88964b` 2026-08-15 - fix(deploy): healthcheck Orion identity socket (#369)
- `bfafbe2` 2026-08-15 - ops: wire WS collapse metric, bound scene-intent dedup cache (OPS-ORION) (#370)
- `44e9468` 2026-08-15 - feat(bluehost): wire core.overlay-app.set@1 to the real LSDP wire (#371)
- `83f437c` 2026-08-15 - fix(api): route operator rail antenna leg to Engine B on-air instance (#372)
- `d6c9cca` 2026-08-15 - test(e2e): wire ZabCanvas CanvasLayout contract into e2e gate (#373)
- `5db5650` 2026-08-15 - docs(runbook): declare Orion rollback posture as FORWARD_ONLY (#374)
- `219c543` 2026-08-15 - test(api): prove chat-driven scene reaches LSML projection (#376)
- `a0dd63c` 2026-08-15 - fix(lsdp): propagate projection metadata through mirror
- `60a6f28` 2026-08-16 - feat(api): contre-validate blue.program.v1 against Engine B (#377)
- `caeea43` 2026-08-16 - fix(bluehost): belt-and-suspenders ValidateProgram against live effects (#378)
- `b316a02` 2026-08-16 - feat(bluehost): execute, not just admit, in validate/program (#379)
- `10b0a8e` 2026-08-16 - feat(api): accept operator/admin on POST /validate/program (#380)
- `ee0aa31` 2026-08-16 - fix(host): scope Prepare's idempotent short-circuit by scene ID (#381)
- `c5a2c25` 2026-08-16 - test(api): repoint #181's databound test on real Blue-compiled bytes (#382)
- `90cc7e7` 2026-08-16 - feat(bluewire): observe forwarded projection identity (#383)
- `4804c82` 2026-08-16 - fix(lsdp): bump lumencast-go pin, repair snapshot identity gap counter (#384)
- `c251cf8` 2026-08-16 - test(bluehost): prove http method normalization by execution (#386)
- `fbdf8c2` 2026-08-16 - feat(contracts): ADR 013 T3 — core.http.request@1 contract-fixture arm (#387)
- `8829137` 2026-08-16 - chore(deps): pin runtime/go to signed tag v0.1.0 (#388)
- `55062a1` 2026-08-17 - fix(operator): route ?target=preview through Engine B (#389)
- `1a35f9f` 2026-08-17 - fix(bluehost): gate dispatchInvocations on Execute mode (#390)
- `89605b5` 2026-08-17 - test(operator): cover 3 Bastion-flagged gaps post preview-Engine-B (#391)
- `897ac92` 2026-08-17 - fix(api): join live armed awaits into pending poll + cockpit contract (#392)
- `1beb849` 2026-08-17 - test(api): cover dual-slot poll isolation for pending/cockpit awaits (#393)
- `4469370` 2026-08-17 - fix(bluewire): de-flake Bridge.Run tests via event-driven wait (#394)
- `0c7b7d8` 2026-08-17 - fix(api): reject unknown ?target= as 400 instead of firing on air (#395)
- `fcc1513` 2026-08-17 - fix(api): close the antenna-bundle harvest, key resolver by scene+digest (#401)
- `0bf57c0` 2026-08-17 - fix(bluehost): Take records sceneID, sceneVersion reaches the client (#402)
- `beddd4d` 2026-08-17 - fix(stateless): route scene-intent LSDP mirror by flux, not fixed wire (#400)
- `2ca9aea` 2026-08-17 - feat(scene-intent): accept program-less refs (Decision A, no-program M6 aligned by construction) (#403)
- `977e0d6` 2026-08-17 - fix(workload): mint {ticket,intent} verbatim, drop Orion-side admit (#404)
- `417b03a` 2026-08-18 - fix(workload): post DelegationProxyRequest on canvas fetch and reconcile (#406)
- `f3b422b` 2026-08-18 - fix(orion): align scene intent workload contract (#407)
- `9dc90a5` 2026-08-18 - fix(scene-intent): verify Blue canonical program digests (#408)
- `c4f84fc` 2026-08-18 - fix(attestation): accept canonical Canvas API locators (#409)
- `60ac4c7` 2026-08-18 - fix(attestation): accept and bind Canvas payload kid (#410)
- `ce34c47` 2026-08-18 - fix(preview): replace the prepared scene slot
- `6911b9f` 2026-08-19 - fix(preview): compile static LSML before Solar (#411)
- `b287e02` 2026-08-19 - fix(preview): compile every static render bundle (#413)
- `dfb3978` 2026-08-19 - fix(preview): accept Canvas bundle metadata identity (#414)
- `a95acc1` 2026-08-19 - fix(preview): seed literals and public asset URLs (#415)
- `4c0698c` 2026-08-19 - fix(preview): seed Canvas defaults for Blue scenes (#416)
- `b05c4d2` 2026-08-19 - fix(preview): activate stateless LSDP scenes
- `55c01a5` 2026-08-19 - fix(preview): execute engine-b scene mutations
- `32f171b` 2026-08-19 - fix(lsdp): bootstrap empty-default scenes
- `6e75e22` 2026-08-19 - fix(preview): announce programmed bundle identity
- `ab3dc8e` 2026-08-19 - fix(lsdp): seed bound leaves before engine deltas
- `c95f6f1` 2026-08-19 - fix: execute read-only DB queries in Engine B preview (#424)
- `db0bb7c` 2026-08-19 - chore(api): expose Engine B operator result diagnostics (#425)
- `8af120d` 2026-08-19 - fix(host): forward Engine B graph outputs (#426)
- `46eeb7f` 2026-08-19 - chore: diagnose empty Engine B projections (#427)
- `fdfc49b` 2026-08-19 - feat: serve validated render capsules
- `a982903` 2026-08-19 - perf: activate validated capsules inline
- `cab2561` 2026-08-19 - perf: project first Blue state synchronously
- `be21bab` 2026-08-19 - perf: use inline workload admission for validated capsules
- `3f2f148` 2026-08-19 - perf: cache validated Blue program handles
- `3ab8371` 2026-08-19 - perf: move initial Blue projection off intent response
- `d573bf8` 2026-08-19 - ci: expose remote Orion build diagnostics
- `4a8e309` 2026-08-19 - perf: cache validated LSML wire surfaces
- `5866e73` 2026-08-19 - perf: gate asynchronous scene projection
- `640a500` 2026-08-19 - perf: decode scene capsule envelope once
- `3b4de64` 2026-08-19 - perf: reuse verified Blue capsule proofs
- `7ff3d55` 2026-08-19 - perf: reuse seeded scene keyframes on reactivation
- `a6fe730` 2026-08-19 - perf: cache Blue cockpit metadata
- `34b8afb` 2026-08-19 - perf: overlap inline admission with capsule verification
- `aac436a` 2026-08-19 - perf: seed wire before activating scene
- `765cf82` 2026-08-19 - perf: publish scene snapshot before intent response
- `6f410a3` 2026-08-19 - perf: preload stateless scene bundles on LSDP
- `32275b6` 2026-08-19 - fix: recover interrupted service token rotation
- `d2c0042` 2026-08-19 - chore: expose engine b output samples
- `5d82fc0` 2026-08-19 - test: guard zero-sequence live projections
- `d83fbec` 2026-08-19 - chore(orion): remove retired scene adapters
- `3773b95` 2026-08-20 - feat(stream-rules): restore global overlay rule plane
- `2105a96` 2026-08-20 - chore(ci): remove retired scene-intent helpers
- `cbdad22` 2026-08-20 - Merge pull request #428 from ZabLaboratory/eleven/153-stream-rules-overlay
- `ca52533` 2026-08-20 - fix: arm on-air peer viewer with scoped service token
- `4c255fd` 2026-08-20 - fix(runtime): execute camera slot extensions on stream rules
- `b042313` 2026-08-20 - build(deps): record Blue runtime checksum
- `6a4173f` 2026-08-20 - fix(bluehost): adapt slot extension route references
- `ba7d243` 2026-08-20 - fix(observability): trace stream rule extension execution
- `5b902c4` 2026-08-20 - fix(bluehost): wire published egress routes at boot
- `7d0fba1` 2026-08-20 - fix(orion): preserve Blue compile diagnostics
- `5ebfcc3` 2026-08-21 - fix(orion): deliver platform events to stateless hosts
- `7b3b061` 2026-08-21 - fix(orion): bound text render nodes from authoring metadata
- `3855fc1` 2026-08-21 - fix(orion): consume updated Blue runtime loader
- `eac78de` 2026-08-21 - fix(orion): project stream-rule show emits to stateless slots (#429)
- `6f8eece` 2026-08-21 - build: consume Blue runtime snapshot fix (#430)
- `1a8ab8d` 2026-08-21 - fix(orion): preserve show emit ordering on rejected topics (#431)
- `ed6b698` 2026-08-22 - docs(orion): require service token exchange grants
- `d6c8443` 2026-08-23 - feat: embed Orion show execution locally
- `8f242b1` 2026-08-23 - Merge Orion embedded-local show execution
- `3e9552b` 2026-08-24 - feat(orion): execute synchronized scenes locally
- `0742ac9` 2026-08-24 - merge: reconcile Orion main with embedded local runtime
- `bf1c1b0` 2026-08-24 - fix(orion): make local scene-intent branches lint clean
- `60a9eee` 2026-08-24 - Merge pull request #433 from ZabLaboratory/eleven/orion-full-local-20260824
- `a35fa45` 2026-08-24 - fix(orion): use service bearer for Blue stream rules (#434)
- `2e3ec41` 2026-08-24 - fix(orion): use durable bearer for Blue stream rules (#435)
- `7f31643` 2026-08-24 - fix(orion): name stream-rule Blue auth failures (#436)
- `fe7f83b` 2026-08-24 - feat(runtime): attest embedded Blue runtime and retire VPS deploy
- `b21846c` 2026-08-24 - Merge pull request #437 from ZabLaboratory/eleven/orion-local-retirement-20260824
- `4f649ad` 2026-08-24 - fix(orion): expose admitted artifact digest per slot
- `e490166` 2026-08-25 - feat(local-runtime): admit exact programs and acknowledge writers
- `667ea0c` 2026-08-25 - chore(build): ignore generated artifacts
- `4624eb4` 2026-08-25 - fix(local-runtime): preserve identical scene generations
- `efd9b18` 2026-08-25 - fix(local-runtime): project platform event outputs
- `3a396cc` 2026-08-25 - release(orion): 2.0.0
## [Unreleased]

### ✨ Features

- **Stream-level Blue rules — promotion API + `show.emit` bridge**
  (ADR 009 §3.6, issues #154/#155). Operator-gated promotion/demotion of a
  roster scene into an always-on stream rule (`POST`/`DELETE
  /api/v1/show/stream-rules`), gated on R9 validation (`SCENE_NOT_VALIDATED`)
  and refusing the active scene (`RULE_IS_ACTIVE_SCENE`); a promoted rule is
  refused archival (`SCENE_IN_USE`). The promoted set is persisted
  (`show_stream_rules`, migration 0005) and reseeded at boot — restart
  restores the same rules (criterion #11). The 82nd Blue primitive
  `core.show.emit@1` gains its Orion executor: it injects `__events.<topic>`
  = payload into the ACTIVE scene only, via a distinct active-only system
  inbox path that never traverses the routing union — a rule's emission can
  never cascade rule→rule (anti-loop by construction). Construction-safe,
  no error pin (delivery to the active scene's system inbox cannot fail).
  Conformance manifest/signatures regenerated off the merged Blue seed (82
  nodes / 68 signatures); the bidirectional `exec-port-parity` gate covers
  `show.emit` on both sides. (`internal/runtime/exec_show_emit.go`,
  `internal/api/stream_rules.go`, `internal/store/show_stream_rules.go`,
  `migrations/0005_show_stream_rules.sql`, `internal/conformance/`)
- **Total-conformance matrix CI gate** (ADR 003 §3.4 phase 5 / §6
  criterion 1, the MASTER criterion — issue #88). A new
  `conformance-matrix` CI job (hard-fail, no `continue-on-error`)
  proves Orion serves the entirety of Blue: for every node id in Blue's
  compute manifest (67 `core.*` + 14 `quasar.twitch.*` = 81), it
  asserts a registered Orion executor exists **and** the executor runs
  through the real engine. The transitional gap set lives in
  `conformance_allowlist.txt` and only ever shrinks — a ratchet test
  fails CI if the list grows. The vendored manifest
  (`internal/conformance/manifest.json`, regenerated by
  `scripts/gen_conformance_manifest.py` from Blue's seeder + purity map)
  lets the matrix run offline. The id→executor classification is
  cross-checked against the real `NewComputeRegistry()` ids and
  `runtime.ExecOps`, so a node claimed served that nobody registered
  fails CI, not air. Executable proof in
  `internal/runtime/conformance_exec_test.go` drives every compute id
  and every exec op through a real registry/Scene (no mocks). Criterion
  2 (one-of-each-type, push→validate→activate→observe) lands as an e2e
  over the compiler-servable subset
  (`tests/e2e/conformance_oneofeach_test.go`). Allowlist at landing: 6
  (the inline-only `core.db.{from,where,join,select,order,limit}@1`
  query-builder atoms, which have no standalone executor by Blue's own
  contract). (`internal/conformance/`, `.github/workflows/ci.yml`)
- **Pure data-node registry tranche — 27 executors** (ADR 003 §3.4
  phase 0, issue #81). The runtime compute registry now serves
  `core.logic.{and,or,xor}`, `core.math.{abs,min,max,clamp,lerp,round,
  floor,ceil}`, `core.string.{concat,format,length,split,upper,lower}`,
  `core.cast.{to-string,to-integer,to-float,to-boolean}` and
  `core.data.{get-field,set-field,list-length,list-at,list-append,
  aggregate}`, each faithful to Blue's reference executor
  (`executor.py`) — Python-truthiness logic, banker's rounding,
  first-argument-wins min/max ties (signed-zero-exact), strict
  integer guard on `list-at`, signature defaults on unwired ports.
  `core.data.get-field` is the phase 2 platform-payload extractor.
  (`internal/runtime/compute_pure.go`)
- **Node config carried into the graph artefact** (issue #81). The
  compiler now copies a computed blueprint node's `config` object onto
  `GraphNode.Config` (additive, `omitempty`), and `ComputeFn` receives
  it — mirroring Blue's `handler(inputs, config, state)` contract so
  config-bearing pure nodes (`get-field`/`set-field` `path`,
  `aggregate` `op`) can execute. Pre-existing artefacts and
  config-less nodes are byte-identical. (`internal/compiler/graph.go`,
  `internal/runtime/compute.go`)

## [1.1.0] - 2026-06-10

Second release of the Go runtime. v1.1.0 is the **Lumencast-convergence
groundwork** release: Orion now emits and serves the cross-language
**LSML 1.1** bundle alongside its bespoke render bundle, accepts an
**N-blueprint** push envelope, and adopts a **upsert-on-push** scene
lifecycle — all behind back-compatible defaults so existing Prism /
Canvas / Blue clients keep working byte-for-byte. The release also
hardens the live service-token plane (mint + rotation against ZabAuth)
and resolves the deploy pipeline that brought the v1.0.0 cut onto the
VPS. No breaking changes: the LSML path is gated behind
`ORION_LSDP_MODE` (default `bespoke` = no-op), migration `0002` is
additive/nullable, and the multi-blueprint envelope serialises a
length-1 list byte-identically to the legacy single-blueprint format.

### ✨ Features

- **LSML 1.1 emission alongside the render bundle** (ADR 007 §C). The
  compiler now produces a second, language-neutral artefact — the
  **LSML 1.1** bundle — in the same compile pass that builds the
  bespoke `RenderBundle`. Go↔TS cross-language golden fixtures prove
  byte-stable parity, so the same scene compiled by either runtime is
  identical. (`internal/compiler/emit_lsml.go`)
- **Orion serves LSML bytes** (ADR 007 §C.2). New
  `GET /api/v1/scenes/{id}/lsml-bundle?v={hash}` endpoint, content-
  addressed and immutable, served from the new nullable
  `lsml_bundle_jsonb` / `lsml_bundle_hash` columns (migration `0002`).
  Orion treats the bundle as opaque bytes — it never walks the layout
  tree; the TS runtime does.
- **Identity adopt-on-verify** (ADR 007 §C C4). When the LSML hash and
  the legacy `scene_version` mint agree, Orion collapses to a single
  scene identity; on mismatch it falls back to the legacy mint, so the
  identity contract never silently diverges.
- **LSDP/1.1 wire seam** (ADR 007 §C.3b). New `internal/lsdp/` package
  mounts the Lumencast Streaming Data Protocol wire over the existing
  WS transport via a header-trust seam, gated by `ORION_LSDP_MODE`
  (`bespoke` | `dual` | `lsdp`). `bespoke` (default) is a full no-op.
- **Multi-blueprint push envelope** (ADR 001). A single push may now
  carry N distinct Blue blueprints (`blueprints[]`) instead of one
  implicit blueprint. The legacy single-blueprint field is still
  accepted and normalised into a one-element list; sending both at
  once is rejected with `ENVELOPE_BLUEPRINT_CONFLICT` (400).
- **Stream-key handover goes live** (ADR 005 §11). The
  `GET /credentials/{id}/stream-key` endpoint flips from its 503 stub
  into a thin proxy to Quasar through ZabGate, forwarding body and
  status verbatim (503 `QUASAR_NOT_WIRED` when the upstream URL is
  unset, 403 anonymous, 200/404 forwarded, 502 on unreachable).
- **Live service-token mint + rotation** (`internal/auth/`). With
  `ORION_OPERATOR_TOKEN` set, Orion mints a service token from ZabAuth
  at boot and a background loop rotates it 5 min before expiry under a
  write lock; without it, falls back to the static `ORION_SERVICE_TOKEN`
  for dev/test. The fetcher now carries a per-request token (no frozen
  placeholder) and fails explicitly rather than going silently
  anonymous.
- **Compiler runtime-vocabulary lowering.** The compiler now lowers
  authoring constructs into the runtime render vocabulary so the
  reactive engine consumes a flat, contract-stable shape:
  - `RenderBundle` props lowered to runtime vocab (ADR 007 §9).
  - Text `fontFamily` + image `size` lowered to render vocab.
  - `animate` envelopes lowered to per-prop transitions, with a flat
    `animate_initial` emitted on lowered nodes (render-bundle ↔ runtime
    contract parity).
  - The `wipe-cover` authoring element lowered to `RenderNode.keyframes`
    (M10 overlay transition).

### 🐛 Fixes

- **Upsert-on-push scene lifecycle** (ADR 002 §3.1-3.3). `POST /push`
  now upserts the scene row (`ON CONFLICT DO UPDATE RETURNING`),
  fixing the 404 a first push hit against the mandatory `scenes` FK
  row. Race-safe and idempotent; Canvas stays the source of truth for
  the scene name.
- **Race-safe `definition_version` on concurrent first-push** (#56) —
  two simultaneous first pushes no longer clobber the version pointer.
- **Operator-input defaults seeded into `graph.Defaults`** (M9/D2) so a
  cold-started scene reseeds its declared operator-input values instead
  of starting blank.
- **Blueprint compute path** — fetch graph, cold-start and intermediate
  persistence fixed end-to-end (#44).
- **Versioned compute registry keys** — runtime `ComputeRegistry`
  re-keyed on `namespace.name@version` so two versions of the same
  compute coexist (#39).
- **Blueprint node contract alignment** with Blue — node body aligned
  to Blue's `config`/`inputs`/`outputs` (#37), node ref bound to the
  canonical `definition` wire field (#36), and Blue's real
  `_compute-manifest` envelope decoded correctly (#31).
- **Blueprint-free scenes tolerated** — a scene with no blueprint no
  longer fails to compile (#29).

### 🔧 Build / CI / Deploy

- **Repeatable Solar bundle deploy** — new `solar-deploy.yml` workflow
  rolls a version-parameterised Solar bundle onto the VPS under
  `ORION_SOLAR_ROOT/<version>/` without a code push; idempotent
  (installed versions skipped). LSDP deploy runbook added.
- **Solar bundle served from host subtree** — deploy serves the `host/`
  subtree of the dual-build Solar tarball (#59); bundles mounted via a
  host volume and fetched on the VPS (#16).
- **Drop VPS PAT from Solar fetch** — the public Solar bundle is now
  pulled over HTTPS instead of an authenticated VPS personal access
  token.
- **Deploy path hardening** — remote paths interpolated instead of
  passed as `bash -s` positional args (#48); git revision/source
  stamped into OCI image labels; OCI provenance labels + a static
  healthcheck probe added to the image.
- **Docker healthcheck activated** on the orion service, with the
  healthcheck binary exiting via `run()`'s int so deferred cleanup runs
  on every path; `start_period` raised to 30 s to cover the goose
  migration boot.
- **`.gitattributes`** added to pin LF line endings on Go sources (#34).

### 📝 Docs

- ADR-001 (multi-blueprint push envelope) accepted (#51).
- ADR-002 (scene lifecycle Canvas↔Orion, upsert-on-push) accepted (#58).
- Solar deploy host/-subtree hotfix traced in a runbook (#60).

### ⚠️ Breaking changes

- None. All new behaviour is additive and back-compatible: the LSML /
  LSDP plane is off by default (`ORION_LSDP_MODE=bespoke`), migration
  `0002` only adds nullable columns, and the single-blueprint envelope
  is still accepted with byte-identical output.

### New configuration

- `ORION_LSDP_MODE` — `bespoke` (default) | `dual` | `lsdp`. Gates LSML
  persistence + the LSDP wire.
- `ORION_OPERATOR_TOKEN` — operator token used to mint/rotate the live
  service token against ZabAuth. Empty → static `ORION_SERVICE_TOKEN`.
- `ORION_SERVICE_PATHS` — CSV of service-token `paths` claims
  (default `quasar.credentials.read`).
- `ORION_QUASAR_BASE_URL` — Quasar base URL for the stream-key proxy
  (empty → 503 `QUASAR_NOT_WIRED`).

## [1.0.0] - 2026-05-02

First release of the Go rewrite per
[ADR 004 — Orion v2 (reactive runtime)](../docs/adr/004-orion-v2-runtime.md).
The v0.x Python implementation (Twitch orchestrator + MediaMTX relay) is
fully retired ; Twitch lives in **Quasar** going forward (ADR 005),
the streaming media plane lives in **Pulsar** (bundled in Prism), and
Orion now exclusively owns the scene compiler + reactive runtime + WS
fan-out. 18/18 chantier resolution criteria covered by tests, deployed
live via the merged CI/Deploy workflow on `main`.

### Added — v2 Go scaffold

- **Reactive runtime in Go** scaffolded on `feature/v2-go-scaffold`
  on 2026-05-02 per
  [ADR 004 — Orion v2 (reactive runtime)](../docs/adr/004-orion-v2-runtime.md).
  All 18 chantier resolution criteria (15 from ADR § 12 + 3 chantier-
  specific) covered by tests; `go test ./...` and
  `go test -tags e2e ./...` green locally.
- **Scene compiler** (`internal/compiler/`) — fetches Canvas + Blue +
  components, validates types/cycles/purity, hoists `operator_inputs`
  with instance-path prefixing, emits graph + render bundle, hashes
  to a deterministic `scene_version`. Cycle detection rejects with
  `CYCLIC_COMPONENT` (criterion 17); impure compute rejects with
  `IMPURE_COMPUTE` (criterion 18).
- **Reactive engine** (`internal/runtime/`) — per-scene goroutine,
  drain-then-compute event loop, topologically-sorted DAG, dirty
  propagation. Singleton `Show` owns the active-scene authority and
  migrates live subscribers between scenes on switch (no WS reset).
  Process-wide `Tick` source for time-based bindings.
  `TestSessionManager` clones graphs for `__test.*` workflows.
- **Adapters** (`internal/adapters/`) — unified inbox with scope check
  + fan-out routing (every scene that declared a binding on the
  target path), HTTP poller with 429 backoff, PG `LISTEN/NOTIFY`
  primitive.
- **WS server** (`internal/ws/`) — `coder/websocket` upgrade,
  per-connection state, drain-then-write loop, server-driven ping at
  60 s idle, sends snapshots on backpressure collapse.
- **HTTP API** (`internal/api/`) — every endpoint from ADR 004 § 2:
  `/scenes/{id}/push` (compile + rollback), `/render-bundle`,
  `/operator-inputs`, `/graph`, `/scenes/{id}/status`, `/show`,
  `/show/active-scene`, `/show/test-sessions`, `/assets/{id}`, the
  preserved `/credentials/{id}/stream-key` (503 until Quasar lands),
  and `GET /static/solar/v{N.N.N}/*`.
- **Persistence** (`internal/store/`) — `pgx/v5` repositories for
  scenes, definitions (kept forever), pushed versions (purged on
  archive), and assets. Migrations under `migrations/` (goose
  format).
- **Auth** (`internal/auth/`) — trust-headers parser
  (`X-Authenticated-User`/`-Role`/`-Paths`) plus a cached ZabAuth
  `/validate` client for show-token revocation checks.
- **Protocol** (`internal/protocol/`) — typed envelopes + golden
  fixtures (byte-stable for criterion 16 — Solar mock-orion suite
  conformance).
- **Deploy + CI** — multi-stage distroless `Dockerfile`,
  `compose.yaml` with `orion-postgres`, GitHub Actions running vet /
  test (race) / build / docker / staticcheck / golangci-lint /
  trufflehog.

### Removed

- **Entire v0.x Python implementation deleted** on 2026-05-02 per
  [ADR 004 — Orion v2 (reactive runtime)](../docs/adr/004-orion-v2-runtime.md).
  `src/`, `tests/`, `alembic/`, `scripts/`, `deploy/`, `Dockerfile`,
  `docker-compose.yml`, `docker-compose.prod.yml`, `pyproject.toml`,
  `uv.lock`, `Makefile`, `alembic.ini`, `.github/`, `.env.example` are
  all gone. `CHANGELOG.md`, `README.md`, `CLAUDE.md`, `.gitignore`
  remain ; v2 scaffolded into the same project repo on
  `feature/v2-go-scaffold`.
- The Twitch concerns (OAuth Helix, IRC chat plumbing, encrypted
  credentials) move to **Quasar** per
  [ADR 005 — Quasar (multi-platform integrations)](../docs/adr/005-quasar-platforms.md).
  No code is migrated verbatim — Quasar reimplements the Twitch
  surface from scratch. User OAuth tokens do not migrate ; operators
  re-authorize once after Quasar lands.
- The streaming media plane (RTMP/WHIP, MediaMTX integration) is
  retired. **Pulsar** (bundled in Prism) pushes RTMP directly to
  Twitch — Orion is no longer in the media path.

### Note

Orion v0.x was the Twitch orchestrator + browser-composed-scene
relay ; ADR 004 explicitly drops every concern except scene
compilation + reactive runtime. Until v2 ships, the entire `/orion/*`
prefix routes to a non-existent upstream and will return 502 from
ZabGate. Prism's broadcast pre-flight surfaces this as a clear
`twitch_credential` failure.

## [0.4.0] - 2026-04-30

**Architectural pivot.** Orion stops being a streaming control plane and
becomes a pure Twitch orchestrator. The broadcast media path moves to
[Pulsar](https://github.com/ZabLaboratory/Pulsar) (bundled in Prism),
which pushes directly to Twitch RTMP without ever touching Orion.

Orion now owns : Twitch credentials (AES-GCM), OAuth Helix flow, and
the IRC chat plumbing scaffold. EventSub, expanded Helix endpoints, and
the new subscriber-driven IRC supervisor land in follow-up PRs.

### Removed

- **`streams`, `stream_metrics`, `stream_destinations`** tables —
  dropped in migration `0006_drop_streaming`.
- **`chat_messages.stream_id`** — chat is channel-keyed only ; analytics
  group by `channel` and `sent_at`.
- **`stream_state` PostgreSQL enum** — gone with `streams`.
- **MediaMTX integration** — `services/mediamtx.py`,
  `services/stream_manager.py`, `services/stream_events.py`,
  `routes/streams.py`, `routes/destinations.py`, `routes/metrics.py`,
  `routes/mediamtx_auth.py`. Container `orion-mediamtx` and
  `mediamtx/mediamtx.yml` retired.
- **`ChatSupervisor`** — was reconciling live streams ↔ IRC connections.
  A subscriber-driven replacement lands in a follow-up PR.
- **Caddy `orion-media.cyell.dev` snippet** — public WHIP/HLS subdomain
  no longer needed.
- **Env vars** : `MEDIAMTX_API_URL`, `MEDIAMTX_WHIP_BASE`,
  `MEDIAMTX_PUBLIC_WHIP_BASE`, `TWITCH_RTMP_BASE`,
  `INGRESS_TOKEN_TTL_SECONDS`, `PUBLIC_BASE_URL`,
  `ORION_PUBLIC_BASE_URL`, `ORION_PUBLIC_WHIP_BASE`.

### Changed

- **`_schema` catalogue** — narrowed to `chat_messages` only.
  `streams`, `stream_destinations`, `stream_metrics` entries gone.

## [0.3.0] - 2026-04-25

Phase 4 — scene-switcher. Streams gain a curated overlay playlist + a
dedicated endpoint to swap the active overlay live, plus a state
WebSocket that fans the change out to every subscribed client (the
broadcaster, mobile companion, Stream Deck plugin via Companion). The
piece that turns Orion from "single-overlay broadcast pipe" into "OBS-
class scene switcher", with WHIP session preserved across switches.

### Added

- **`streams.overlay_playlist`** — JSONB column carrying the list of
  overlay ids the operator can swap to live. Pre-declared at stream
  creation (or via `PUT /streams/{id}` while not LIVE), so a stale
  macro request can't activate an arbitrary overlay and blank the
  broadcast.
- **`POST /api/v1/streams/{id}/active-overlay`** — focused endpoint
  designed to fire mid-broadcast. Validates that the new overlay id
  is in the stream's playlist (or `None` to clear → raw camera
  fallback). Unlike `PUT /streams/{id}` which refuses while LIVE,
  this one is *the* live-edit path. Persists, commits, then publishes
  an `active_overlay_changed` event.
- **`WS /api/v1/streams/{id}/state`** — live event stream for one
  stream. Forwards every JSON frame published on
  `stream_event_bus`. Today carries `active_overlay_changed` and
  `state_changed` (lifecycle); new event types are additive — clients
  ignore unknown `event` strings.
- **`StreamEventBus`** — in-process pub/sub primitive (mirrors the
  existing `ChatEventBus`). Per-stream queues with overflow drop-
  oldest semantics — slow subscribers can't pin RAM. Replace with
  Redis pub/sub if Orion ever scales beyond one replica.

### Changed

- `start_stream` / `stop_stream` routes now publish `state_changed`
  events on commit so subscribers see lifecycle transitions on the
  same channel as overlay switches — one socket, both feeds.
- `StreamCreate` / `StreamUpdate` / `StreamRead` / `StreamSummary`
  carry `overlay_playlist`. UUID values are serialised to strings on
  disk (JSONB-friendly) and coerced back at the schema layer.
- Reversible alembic migration `0003_playlist` adds the column with
  server default `'[]'::jsonb` so existing rows pick up the new shape
  without a backfill step.

### Notes

- Phase 4 unlocks two follow-ons: the **mobile companion app**
  (Capacitor + the same Orion API) and **macro integrations**
  (Stream Deck plugin or Bitfocus Companion module). All three
  surfaces consume the same operator command set —
  `start`/`stop`/`active-overlay` over HTTP plus the state WS.
- The renderer-side change (broadcaster re-mounts overlay on switch
  while preserving the WHIP MediaStream) ships in Prism v0.12.0.

## [0.2.0] - 2026-04-25

<!-- commits-since: v0.1.0 -->

### Changed (breaking)

- **Architectural pivot — Orion no longer authors scenes.** Visual
  composition lives in ZabCanvas (`/canvas/api/v1/overlays`); blueprint
  components are hydrated by Blue at render time. Orion shrinks down to
  what's intrinsically a streaming concern: credentials, stream
  lifecycle, MediaMTX orchestration, Twitch IRC pump.
  - **Dropped tables**: `scenes`, `chat_components`. Their associated
    routes, services, and models are gone (`/api/v1/scenes/*`,
    `/api/v1/chat/components/*`).
  - **Streams** now point at a ZabCanvas overlay via a soft pointer:
    `streams.scene_id` (FK) → `streams.overlay_id` (UUID, nullable, no
    cross-service FK). Orion never dereferences it — the renderer
    (Prism / ZabView) is the one that fetches and composes.
  - **Migration `0002_pivot`** drops scenes/chat_components, drops and
    recreates streams + chat_messages + stream_metrics. Existing
    streams/scenes/chat data was scaffold and is not preserved.

### Added

- **Streaming parameters on the Stream row** — Orion is now the control
  panel for every encoder knob. New columns drive the ffmpeg transcode:
  `target_width`, `target_height`, `target_fps`, `video_bitrate_kbps`,
  `audio_bitrate_kbps`, `keyframe_interval_s`, `encoder_preset`. Default
  matches Twitch Partner-tier 1080p30 6 Mbps with a 2 s keyframe
  interval.
- **`PUT /api/v1/streams/{id}`** — patch streaming parameters or the
  overlay reference between sessions. Forbidden while LIVE/PREPARING
  (would silently drift from the running ffmpeg child).
- **`build_twitch_relay_config(stream_key, path, params)`** — ffmpeg
  command line is now templated from a `StreamingParams` dataclass
  instead of being hardcoded. Tests covering the rendered command stay
  green.

### Kept

- Twitch credentials (encrypted stream keys + OAuth tokens), MediaMTX
  external auth webhook, OAuth loopback flow, chat IRC supervisor +
  `/api/v1/chat/live/{channel}` WS (Blue blueprints subscribe over WS
  for chat-driven components), `chat_messages` capture for
  replay/audit.

## [0.1.0] - 2026-04-24

### Added

- **First release.** Orion is now live at
  `https://zabgate.cyell.dev/orion/*`, MediaMTX media endpoint at
  `https://orion-media.cyell.dev`, paired client shipped in Prism
  v0.3.1.
- **Scenes.** CRUD canvas scenes (sources: webcam / screen / image /
  text / color / iframe / canvas-overlay, audio mixer). Config is
  opaque JSONB so the editor can grow without migrations.
- **Twitch credentials.** Stream keys + optional OAuth tokens
  AES-GCM-encrypted at rest; only presence flags ever leave the API.
- **Stream lifecycle.** `pending → preparing → live → stopping →
  ended | error`. `stream_manager` is the single writer — the only
  module that mutates state and the only one that talks to MediaMTX.
- **MediaMTX orchestration.** Dynamic path provisioning via the v3
  control API; `runOnReady` spawns ffmpeg on first publish to
  transcode WebRTC VP8/Opus into H264/AAC at 6 Mbps, 2-second
  keyframes (Twitch's hard requirement) and pushes to RTMP.
- **External auth webhook.** MediaMTX calls `/mediamtx/auth` on every
  publish; Orion verifies the single-use ingress token from the WHIP
  query string, flips the stream to LIVE, and rejects the session
  otherwise. Reads stay permissive (paths are unguessable UUIDs).
- **Twitch OAuth.** Two-leg flow with HMAC-signed state envelope.
  Accepts any `http://localhost:*` loopback redirect URI (for the
  Prism desktop flow) or the configured web URI, signs it into state,
  and echoes it byte-for-byte on token exchange (RFC 6749 §4.1.3).
- **Twitch chat prep.** IRC client + `ChatSupervisor` lifespan task
  that reconciles live streams against IRC connections every 10 s,
  pumps messages into `chat_messages` (for replay) and the in-process
  bus (for live WS fan-out).
- **Chat components.** CRUD for blueprint-backed overlays
  (`blueprint_ref` points at Blue). Orion owns placement + triggers,
  Blue owns rendering.
- **Live metrics WS.** `/api/v1/metrics/streams/{id}/live` polls
  MediaMTX every 2 s and streams state transitions + runtime snapshot
  to subscribed clients until the stream ends.

### Infrastructure

- Postgres 16 schema (6 tables + `stream_state` enum) via Alembic.
- Gateway-first: all control-plane traffic routes through
  `zabgate.cyell.dev/orion/*` (JWT validated at the gateway,
  `X-Authenticated-User` injected into every upstream call).
- MediaMTX container pinned to `bluenviron/mediamtx:latest-ffmpeg`
  (plain `latest` ships without ffmpeg).
- Deploy workflow uses SCP + a runner-side rendered `.env` instead of
  an indented heredoc over SSH so the Windows-runner path mangling
  can't leak into the script.
