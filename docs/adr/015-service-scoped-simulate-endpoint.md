# ADR 015 — Service-scoped simulate endpoint

- **Status:** accepted (**Amendment 1**, 2026-06-16)
- **Date:** 2026-06-16
- **Decided:** 2026-06-16
- **Deciders:** @ClodoCapeo
- **Author:** Atlas
- **Supersedes:** —
- **Superseded by:** —

> **⚠️ Amendment 1 (2026-06-16) en vigueur.** Le contrat de requête de §3.2 et le
> flux d'exécution de §3.3 ont été **corrigés** : l'entrée n'est PAS un `compiler.Graph`
> pré-compilé mais le **graphe d'authoring Blue** (`BlueprintGraph`), qu'Orion **compile
> in-body** avant `Harness.Simulate`. Lire §3.2/§3.3 **à travers** la section
> [Amendment 1](#amendment-1--2026-06-16--compilation-in-body-du-graphe-blue) en fin de
> document — elle prime sur le texte original là où ils divergent. Le reste de l'ADR
> (transport REST §3.1, auth/scope §3.2, isolation B10, budget, descope `blueprint_ref`)
> est inchangé.

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

  > **Risque résiduel accepté (Bastion, clearance #198) :** `/validate/simulate` est
  > CPU-bound et exécute un graphe fourni par l'appelant, borné par-requête (steps/wall/413)
  > mais sans throttle par-débit (Q1). Accepté pour le MVP — surface limitée au seul
  > service-token `bluemcp-agent` derrière ZabGate, exécution synchrone single-goroutine
  > bornée, aucune amplification. Throttle par token (token-bucket Orion ou limite ZabGate)
  > à livrer en suivi, sans bloquer ce merge.
- **Q2 — `blueprint_ref` (phase 2b).** Garde-t-on la résolution d'une version poussée, et
  avec quelle gate scope additionnelle ? Descope par défaut ici. **(Amendment 1 : reste
  descope — l'Option A, graphe in-body compilé par Orion, rend `blueprint_ref` non
  nécessaire pour le MVP simulate ; il n'apporterait qu'un accès store/read au
  service-token, repoussé à 2b avec sa gate additionnelle.)**

---

## Amendment 1 — 2026-06-16 — Compilation in-body du graphe Blue

### A1.1 Cause racine (bug constaté en prod, post-#198)

L'endpoint `POST /api/v1/validate/simulate`, mergé tel quel (#198), **ne produit jamais
de rapport per-entrypoint** (`blueprints: null`) en usage réel. Diagnostic (Lens,
preuves file:line) :

- `postSimulate` (`internal/api/validate_simulate.go:87-92`) décode le corps `graph`
  dans un `compiler.Graph` (`json.Unmarshal`). Tous les champs de `compiler.Graph` sont
  `omitempty` → le décode **réussit silencieusement** sur un graphe Blue, mais le champ
  `ExecPrograms` reste **vide** (rien dans le JSON Blue ne le porte).
- `runtime.ExecProgramsFromGraph` (`validation_harness.go:59-72`) voit
  `len(graph.ExecPrograms) == 0` → retourne `(nil, nil)`. `Harness.Simulate` boucle alors
  sur **zéro programme** → `blueprints: null`. Aucune erreur, aucun signal.

**Hypothèse fausse de l'ADR original (§3.2 ligne 121, §3.3 step 1-2).** Le commentaire
« `compiler.Graph` draft, déjà compilé par Blue » est **faux**. Blue ne produit pas de
`compiler.Graph` Orion : il produit un **graphe d'authoring** (`Blue/src/blue/schemas/
graph.py` → `{nodes:[{id, definition, config, inputs, outputs}], edges:[{from_node,
from_port, to_node, to_port}], variables}`). Le champ `compiler.Graph.ExecPrograms`
n'est rempli **que par le compilateur Orion** au push de scène
(`internal/compiler/exec_partition.go::partitionBlueprint` → `marshalExecProgram`,
assigné en `compile.go:312`). **L'étape de compilation manquait** dans le chemin simulate.

Les tests #198 construisaient des `ExecProgram` directement en Go et ne passaient jamais
par le décodage d'un graphe Blue authoring-level — d'où le faux sentiment de couverture
(cf. A1.5).

### A1.2 Décision — Option A : compilation in-body (remplace §3.2 ligne 121, §3.3 step 1-2)

L'endpoint accepte le **graphe d'authoring Blue**, le **compile dans Orion** via la chaîne
de compilation existante, puis appelle `Harness.Simulate`. Options écartées : **B**
(`blueprint_ref` → resolve d'une version poussée = read store + egress + gate scope
additionnelle + clearance Bastion) ; **C** (compilateur en Python côté Blue/bluemcp =
drift de sémantique, viole D3/R5 et R6 ADR 003). L'Option A ne touche aucune surface
sensible nouvelle (cf. A1.4).

**Forme d'entrée exacte (corrige le bloc requête §3.2).** Le champ `graph` est un
`BlueprintGraph` (`internal/compiler/types.go:208-225`, miroir de
`Blue/src/blue/schemas/graph.py`), tel que `create_draft_version` le produit :

```jsonc
{
  "graph": {
    "id": "…",
    "nodes": [
      { "id": "n1", "definition": "core.input@1", "config": { "name": "…" },
        "inputs": [ /* BlueprintPort: name,type,kind(data|exec),default */ ],
        "outputs": [ … ] }
      // … core.* uniquement ; PAS de noeud `reference` en MVP (cf. A1.3 c)
    ],
    "edges": [ { "from_node":"n1","from_port":"out","to_node":"n2","to_port":"in" } ],
    "variables": [ { "id":"v1","name":"…","type":"…","value": … } ]
  },
  "synthetic_event": { "topic": "chat", "payload": { … } },
  "entrypoints": [ "optional/explicit/keys" ]
}
```

Le champ `graph` se décode désormais dans un **`compiler.BlueprintGraph`**, jamais dans un
`compiler.Graph`. La clé blueprint scene-local par défaut est `""` (legacy single-key,
ADR 001 §3.2) — un seul blueprint draft par requête en MVP.

### A1.3 Flux d'exécution (remplace §3.3 step 1-3)

1. Décoder `graph` dans un `compiler.BlueprintGraph`. Échec JSON ou `nodes` vide →
   **400 `INVALID_GRAPH`** (inchangé en code, corrigé en cible de décode).
2. **Compiler in-body, sans Fetcher (zéro egress).** Nouveau seam exporté dans le package
   `compiler` (ex. `CompileExecPrograms(bp *BlueprintGraph, key string)
   ([]json.RawMessage, *CompileError)`) qui exécute la partition exec existante
   (`partitionBlueprint` + `validateExecTargets` + `marshalExecProgram`) **sans** appeler
   `Compile()` (lequel exige un `Fetcher` HTTP vers Blue/Canvas — l'egress qu'on refuse).
   La partition n'a besoin **que** du graphe in-body : elle lit `node.definition`,
   `inputs/outputs[].kind`, `edges`, `variables` — pas de manifest réseau. Le résultat
   alimente `graph.ExecPrograms` d'un `compiler.Graph` minimal (data-tranche vide : le
   rendu n'est pas exercé en simulate).
3. **Comportement d'erreur de compilation (nouveau).** Toute diagnostic error-severity de
   la partition → **400 `COMPILE_FAILED`** avec la liste des diagnostics
   (`{code, message}` par item), **distinct** de `INVALID_GRAPH` (corps mal formé) et de
   `INVALID_BUNDLE`. Couvre au minimum :
   - noeud `definition` inconnu / non servi → diagnostic compilateur ;
   - cible exec danglante (`EXEC_UNKNOWN_NODE`, `validateExecTargets`) ;
   - cycle de composant (`CYCLIC_COMPONENT`) le cas échéant ;
   - **noeud `reference` présent** (ADR 014) : l'expansion exige un `Fetcher` (resolve
     d'une version poussée) → **descope MVP**, rejeté `COMPILE_FAILED` code
     `REFERENCE_NOT_SUPPORTED`. Plus jamais de `blueprints: null` muet.
4. `progs, _ := runtime.ExecProgramsFromGraph(graph)` lit maintenant un `ExecPrograms`
   **non vide** ; `Harness.Simulate(graph, bundle, progs, syntheticEvent, entrypoints)`
   tire les entrypoints (inchangé). Sérialise le `ValidationReport` → **200**, `blueprints`
   non-null.

### A1.4 Invariants préservés (aucune surface sensible nouvelle)

- **Isolation B10 / validation-mode :** inchangée — le seam compile produit des
  `ExecProgram` ; l'exécution reste `Harness.Simulate` sur clone validation-mode, seam
  d'effet inerte. `ValidateValidationModeCoverage()` reste appelée avant exécution.
- **Budget steps/wall, borne de corps (`maxSimulateBody` 1 MiB → 413) :** inchangés. La
  compilation in-body est bornée par la taille du corps (un graphe draft reste petit).
- **Gate exact-scope `orion.validate.session` (fail-closed, `hasExactScope`) :** inchangée.
- **Pas d'egress réseau nouveau :** le seam compile **n'utilise pas de `Fetcher`** ; il ne
  touche ni Blue, ni Canvas, ni le store. `reference` (qui exigerait un resolve réseau)
  est explicitement rejeté. **C'est l'invariant clé qui maintient la clearance Bastion
  #198 valable** : la surface reste « exécuter un graphe fourni par l'appelant, borné,
  zéro-effet » — la compilation in-body est du **CPU pur sur l'entrée déjà bornée**, pas
  une nouvelle capacité réseau ni store. → **Pas de nouvelle clearance Bastion requise.**
  (Si une itération future réintroduisait `reference`/`blueprint_ref` avec resolve réseau
  ou read store, **cela** rouvrirait R4 et exigerait Bastion — hors de cet amendement.)
- **Pas de persistance :** aucun record `scene_validations`, aucun reload roster (inchangé).

### A1.5 Resolution criteria — ajout (ferme le trou de couverture)

Les critères §6.1-6.6 restent valides. **R3 (§6.3) est renforcé** et un **R7** est ajouté :

7. **R7 (compile in-body end-to-end — trou de couverture #198).** Un **graphe Blue
   authoring-level en JSON** (forme `BlueprintGraph` `{nodes,edges,variables}` avec au
   moins un entrypoint on-event/on-start et un noeud exec, tel qu'émis par
   `create_draft_version` — **PAS** un `ExecProgram` construit en Go) envoyé à l'endpoint
   produit une réponse 200 dont `blueprints` est **non-null**, avec **un `EntrypointResult`
   par entrypoint tiré** (steps, wall_ms, leaves_written, effects_attempted,
   exec_node_coverage, pass/fail_reason). Ce test **doit** décoder via `BlueprintGraph` et
   passer par la compilation in-body — c'est exactement le chemin que #198 ne testait pas.
   En complément :
   - un graphe avec une `definition` inconnue, une cible exec danglante, ou un noeud
     `reference` → **400 `COMPILE_FAILED`** (jamais `blueprints: null`, jamais 200 muet) ;
   - le code `COMPILE_FAILED` est **distinct** de `INVALID_GRAPH` (corps mal formé /
     `nodes` vide → 400 `INVALID_GRAPH`).
