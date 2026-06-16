# ADR 015 — Service-scoped simulate endpoint

- **Status:** accepted
- **Date:** 2026-06-16
- **Decided:** 2026-06-16
- **Deciders:** @ClodoCapeo
- **Author:** Atlas
- **Supersedes:** —
- **Superseded by:** —

## 1. Context

`bluemcp` (ADR Blue 004) — serveur MCP d'authoring de blueprints par un agent
Claude — a livré son MVP et la quasi-totalité de la phase 2. Son **dernier outil,
`simulate(scene_or_blueprint, synthetic_event) → rapport per-entrypoint`** (ADR Blue
004 §3.3 E), bute sur l'absence de surface Orion adéquate. Le porteur a tranché :
**faire évoluer Orion**, pas hacker Blue.

L'appelant est `bluemcp-agent`, un **service-token** (`role=service`) dont le scope
`paths` inclut `orion.validate.session` et **jamais** `blue.blueprint.publish`. Il
veut prouver l'exécutabilité d'un blueprint *draft* contre un event synthétique, à
**zéro effet externe**.

Trois manques sur la surface actuelle (constatés sur `origin/main`) :

1. **Auth.** `POST /api/v1/show/test-sessions` est `requireOperator`
   (`internal/api/show.go:113`, `requireOperator` à `internal/api/public.go:147`) —
   operator/admin only ; un service-token → **403**. Aucune gate par scope sur cette
   route. Même angle mort plateforme que `zabgate-paths-not-enforced` : ZabGate
   injecte `X-Authenticated-Paths`, l'upstream (Orion) ne le lit pas ici.
2. **Pas d'event synthétique à l'open.** `POST /show/test-sessions` ne prend qu'un
   `scene_id` (scène **déjà poussée** au roster) ; les events s'injectent ensuite via
   le WS `__test.*` (`internal/ws/server.go:75 ServeTestSession`). Pas adapté à un
   draft non poussé ni à un one-shot synchrone.
3. **Pas de rapport synchrone.** Le rapport per-entrypoint n'est observable que sur
   le WS (flux de snapshots) ; aucune réponse REST structurée.

**Ce qui existe déjà et ne doit pas être réinventé** — l'isolation et le rapport :

- `internal/runtime/validation_harness.go` : `Harness.Validate(graph, bundle, progs)`
  est **synchrone**, tourne chaque programme dans **son propre clone validation-mode**
  (état privé, zéro subscriber, seam d'effet inerte), et **retourne** un
  `ValidationReport{ Blueprints[].Entrypoints[] EntrypointResult }`. Le `EntrypointResult`
  (`exec_validation.go:280`) porte déjà : `entrypoint, kind, event, pass, fail_reason,
  steps, wall_ms, leaves_written, effects_attempted, exec_node_coverage` — **exactement**
  le rapport que `simulate` demande.
- **Zéro-effet structurel (B10).** `SetValidationMode()` route tout op world-touching
  (`exec_effects.go worldEffectOps`) vers le **seam inerte unique** ; `ValidateValidationModeCoverage()`
  (`exec_validation.go:101`) est un garde réflexif qui échoue le build si un effet ajouté
  plus tard n'a pas de comportement validation-mode déclaré. Aucun broadcast, aucune
  mutation du show actif, aucun envoi plateforme ne peut sortir d'un clone validation.
- **Précédent de gate-par-scope fail-closed.** `internal/api/exec_completion.go`
  (`hasExactScope`, scope `__system.anim.report`, contrat Conduit #86/#124) gate une
  route par **appartenance exacte au scope** (pas de match hiérarchique parent/wildcard),
  fail-closed, drop silencieux indistinguable du succès.

Le travail est donc **d'exposition**, pas d'invention de moteur.

## 2. Decision drivers

- **D1 — Moindre privilège.** Un service-token de validation ne doit jamais accéder
  aux vraies routes show operator-only ni gagner un droit d'écriture live.
- **D2 — Zéro-effet prouvé, pas promis.** Le clone doit être structurellement incapable
  d'émettre vers le live/la plateforme — réutiliser B10, le tester.
- **D3 — Réutilisation maximale.** S'appuyer sur `Harness.Validate` et `EntrypointResult` ;
  pas de second interpréteur ni de sémantique de validation parallèle (anti-drift, R6 ADR 003).
- **D4 — Draft non poussé.** L'agent simule un blueprint *en cours*, pas une scène déjà
  au roster — le graphe doit pouvoir venir dans le corps de la requête.
- **D5 — Modèle réactif d'Orion.** Une simulation = fire d'un (ou des) entrypoint(s)
  ciblé(s) sous budget borné ; ce qui termine sous budget est synchrone par nature.
- **D6 — Continuité de doctrine.** Réutiliser le pattern de gate scope déjà audité (#86),
  pas en inventer un nouveau.

## 3. Decision

### 3.1 Transport : REST synchrone (pas WS)

Une simulation `simulate` est une requête **request/response one-shot** : fournir un
graphe + un event, recevoir un rapport. `Harness.Validate` est déjà **synchrone et
bornée** (budget 5 s / 1 M steps par défaut, `exec_validation.go`). Le WS `__test.*`
sert un usage différent — itération interactive longue d'un *operator* sur une scène
**poussée**. L'imposer à l'agent ajouterait un protocole de session (open → upgrade →
inject → lire snapshots → fermer) sans bénéfice : l'agent ne fait pas d'itération
streaming, il veut un verdict. **REST synchrone.**

> Nuance vs le campaign existant (`postValidate`, `scenes_validate.go`) qui est **async
> 202 + poll** : ce dernier valide une *scène poussée entière* (toutes fixtures
> canoniques, persistance d'un record d'air-eligibility, re-load du roster). `simulate`
> ne vise **ni l'air-eligibility ni la persistance** — c'est un dry-run d'auteur sur un
> draft. Le budget borné rend le synchrone sûr ; pas de record écrit. Les deux
> coexistent sans se gêner.

### 3.2 Endpoint

```
POST /api/v1/validate/simulate
```

Route **séparée** de l'arbre `/show/*` (operator-only) et de `/scenes/{id}/*` (scène
poussée). Préfixe `/validate/*` = surface de validation service-scopée. **Pas** sous
`/show` → aucun risque de fuite de la gate operator.

**Auth (gate par scope, fail-closed).** Wrapper `requireServiceScope("orion.validate.session")`
réutilisant le pattern `hasExactScope` (#86) :

- `id := auth.FromHeaders(r.Header)` ; exiger `id.Role == RoleService` **ET**
  `hasExactScope(id, "orion.validate.session")` (appartenance **exacte** au scope —
  ni parent, ni wildcard, conformément au défaut documenté `probe_test.go`).
- Échec → **403** `{"code":"FORBIDDEN"}`. Pas de drop-silencieux-style-#86 ici : #86
  cache l'existence d'une continuation parkée (anti-probe) ; `simulate` n'a pas de
  secret à protéger côté existence — un 403 franc est correct et plus debuggable pour
  l'agent. (Choix tranché ; cf. §5 R3.)
- `X-Authenticated-Paths` est l'unique source du scope (header de confiance ZabGate).
  **Fail-closed** : header absent ou role≠service ⇒ refus.

**Requête.**

```jsonc
{
  // exactement UN des deux :
  "graph":         { /* compiler.Graph draft, déjà compilé par Blue */ },
  "blueprint_ref": { "scene_id": "uuid", "scene_version": "..." }, // optionnel, phase 2b

  "synthetic_event": {
    "topic":   "chat",                  // type quasar.* canonique
    "payload": { "user":"_a", "text":"hi" }  // libre ; défaut = CanonicalEventFixtures[topic]
  },
  "entrypoints": ["optional/explicit/keys"] // défaut : tous les on-event matchant topic + on-start
}
```

- **Phase 2a (MVP de cet ADR) : `graph` in-body uniquement.** L'agent envoie le graphe
  draft que Blue a compilé. Pas de lookup d'une version poussée → pas d'accès store →
  surface d'attaque minimale (rien à exfiltrer, aucun scene_id d'autrui à sonder).
- `blueprint_ref` (résolution d'une version poussée) est **descope phase 2b** (issue
  séparée, gate par scope additionnel `orion.validate.session.ref` si retenu) — évite
  d'ouvrir un read store au service-token dans ce premier jet.
- Borne dure sur la taille du corps (`maxSimulateBody`, ex. 1 MiB) — un graphe draft
  reste petit ; au-delà → 413.

**Réponse 200.**

```jsonc
{
  "harness_version": "1",
  "status": "validated|failed",
  "budget": { "max_steps": 1000000, "max_wall_ms": 5000 },
  "blueprints": [
    { "blueprint_key": "...", "entrypoints": [ /* EntrypointResult tel quel */ ] }
  ]
}
```

C'est le `ValidationReport` existant **sérialisé verbatim** — `simulate` consomme la
même forme que le record de campagne. Pas de nouveau type de rapport.

### 3.3 Exécution : `Harness.Validate` ciblé event

L'endpoint :

1. Décode `graph` (+ `bundle` vide ou minimal — le rendu n'est pas exercé en simulate).
2. `progs, err := runtime.ExecProgramsFromGraph(graph)` — fail-loud si artefact corrompu (400 `INVALID_GRAPH`).
3. Appelle le harness en **mode event-ciblé** : un nouveau seam
   `Harness.Simulate(graph, bundle, progs, syntheticEvent, entrypoints)` qui, au lieu de
   balayer toutes les `CanonicalEventFixtures` × `onTickFrames`, **fire les entrypoints
   sélectionnés avec le payload fourni** (défaut fixture canonique du topic). Réutilise
   `RunValidationEntrypoint` (`exec_validation.go:346`) — même clone validation-mode,
   même budget, même `EntrypointResult`. C'est un **mode de tir restreint** du harness,
   pas un nouveau moteur.
4. Sérialise le `ValidationReport` → 200. **Aucune persistance**, **aucun reload roster**,
   **aucun record `scene_validations`**.

Le clone reste `SetValidationMode()` → B10 → zéro-effet. Le garde
`ValidateValidationModeCoverage()` couvre déjà cette voie (même seam).

## 4. Consequences

- `bluemcp simulate` (ADR Blue 004 §3.3 E) a une cible HTTP réelle, service-scopée,
  synchrone, rendant le rapport per-entrypoint attendu — débloque le dernier outil
  phase 2.
- Une **nouvelle surface de gate-par-scope** (`requireServiceScope`) entre dans Orion ;
  premier scope `orion.validate.session`. Réutilisable pour de futures routes service.
- Orion lit enfin `X-Authenticated-Paths` sur une route HTTP (ferme localement l'angle
  `zabgate-paths-not-enforced` pour cette surface ; ne le ferme pas globalement — autres
  routes operator inchangées).
- `Harness` gagne un mode `Simulate` (tir event-ciblé) à côté de `Validate` (campagne
  complète) ; partage `RunValidationEntrypoint` — pas de duplication d'interpréteur.
- Surface d'attaque ajoutée : un endpoint qui **exécute du code fourni par l'appelant**
  (graphe draft) dans le moteur. Bornage = budget harness (CPU/steps) + B10 (zéro egress)
  + taille de corps. **Clearance Bastion requise** (cf. §5).

## 5. Risks

- **R1 — Exécution de code arbitraire fourni par l'appelant.** Le graphe vient du
  service-token. Mitigation : (a) budget borné (5 s / 1 M steps, divergence → fail, jamais
  de hang) ; (b) B10 zéro-effet (pas d'egress, pas de socket, pas de DB réelle) ; (c)
  `ExecCPUSeconds`/time-slicing protègent le host ; (d) borne de corps. **À clearer Bastion.**
- **R2 — DoS par boucle de simulate.** Un agent en boucle peut saturer le CPU (campagnes
  CPU-bound). Mitigation : throttle/budget **par token** — rejoint la question ouverte ADR
  Blue 004 §7 (« budget/throttle de simulate »). **Non résolu dans cet ADR** → §7 Q1.
- **R3 — Gate scope mal posée = privilege escalation.** Si `requireServiceScope` honorait
  un scope parent/wildcard, un token `orion.*` ou `orion.validate.*` passerait. Mitigation :
  **exact-scope via `hasExactScope`** (défaut explicitement testé contre le match
  hiérarchique, `probe_test.go`). Resolution criteria #R3.
- **R4 — Fuite de la surface store.** Si `blueprint_ref` était implémenté sans gate
  additionnelle, le service-token pourrait sonder des versions poussées d'autres scènes.
  Mitigation : **descope phase 2b**, `graph` in-body only en 2a ; aucun accès store.
- **R5 — Drift de sémantique validation vs live.** Mitigation : même interpréteur, même
  registry, même `RunValidationEntrypoint` que la campagne et que le live (R6 ADR 003).

## 6. Resolution criteria

1. **R1 (auth).** `POST /api/v1/validate/simulate` avec `role=service` + scope **exact**
   `orion.validate.session` → 200. Sans le scope, ou scope parent (`orion.validate`,
   `orion.*`), ou wildcard, ou role≠service, ou header `X-Authenticated-Paths` absent →
   **403** (test fail-closed + test anti-parent-scope, calqués sur `gate_probe_test.go`/
   `probe_test.go`).
2. **R2 (operator inchangé).** Un operator/admin **n'est pas** requis ni élevé sur cette
   route ; les routes `/show/*` restent operator-only (régression-guard).
3. **R3 (contrat).** Un `graph` draft valide + `synthetic_event{topic,payload}` → réponse
   `ValidationReport` avec un `EntrypointResult` par entrypoint tiré (steps, wall_ms,
   leaves_written, effects_attempted, exec_node_coverage, pass/fail_reason). Graphe corrompu
   → 400 `INVALID_GRAPH`. Corps > borne → 413.
4. **R4 (zéro-effet, testé).** Un graphe draft dont un entrypoint tente un op
   world-touching (broadcast, envoi plateforme, write live, DB réelle) est exécuté en
   simulate **sans aucun effet observable** : pas de frame broadcastée, pas de mutation du
   show actif, pas d'egress réseau. Test d'isolation dédié + `ValidateValidationModeCoverage()`
   vert. Aucun `scene_validations` record écrit, aucun reload roster déclenché.
5. **R5 (synchrone borné).** Un graphe à entrypoint divergent (`while` sans sortie) →
   réponse `status=failed` sous budget (pas de hang), `fail_reason` = budget crossing.
6. **R6 (consommation Blue).** `bluemcp simulate` appelle cet endpoint et restitue le
   rapport à l'agent (preuve d'intégration end-to-end côté ADR Blue 004 §3.3 E).

## 7. Open questions

- **Q1 — Budget / throttle par token (R2).** Quelle limite de débit/CPU par service-token
  sur `/validate/simulate` ? Rejoint ADR Blue 004 §7. Options : token-bucket en mémoire
  Orion (process-local, simple) vs limite côté ZabGate (transverse). **Arbitrage porteur /
  à cadrer avec Conduit** ; peut être livré en suivi sans bloquer le MVP de cet ADR.
- **Q2 — `blueprint_ref` (phase 2b).** Garde-t-on la résolution d'une version poussée, et
  avec quelle gate scope additionnelle ? Descope par défaut ici.
