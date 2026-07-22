# ADR 017 — Compile plein-graphe des stream-rules blueprint-direct

- **Status**: accepted
- **Date**: 2026-07-22
- **Decided**: 2026-07-22
- **Deciders**: @ClodoCapeo
- **Author**: Atlas
- **Supersedes**: —
- **Superseded by**: —

## 1. Context

Bug préexistant (#287, rendu atteignable par le fix contrat #294) : les
stream-rules **blueprint-direct** perdent tout leur plan de données à la
compilation. Diagnostic Conduit, vérifié dans le code :

- `promoteBlueprintStreamRule` et `ReloadBlueprintStreamRules`
  (`internal/api/stream_rules.go:220,266`) compilent via
  `compiler.CompileExecPrograms` — le **seam simulate d'ADR 015**, qui ne
  compile que la spine exec et jette le plan data **par conception**
  (« clones a Graph whose data tranche is EMPTY »).
- Le `Graph` promu est construit à la main avec `ExecPrograms` +
  `Defaults` seulement (`stream_rules.go:229-233`) : `Nodes` nil →
  `Scene.nodeIdx` vide au runtime → tout node exec dont un input est
  alimenté par un nœud data-plane câblé (edge normal) ne résout rien —
  échec **silencieux** (`pullData`/`demandValue` retombent sur rien).
- Seules 3 formes d'authoring survivent : littéral foldé en `Config`,
  payload on-call (data-out exec), variable déclarée
  (`foldDeclaredVariables` → `Defaults`). Toute autre source data-plane
  est droppée.
- Symptôme concret (Marker) : `core.overlay-app.set@1` ne résout pas
  `app_id` → `execOverlayAppSet` logge « without app_id — emission
  skipped », fire `then`, n'appelle jamais `EmitOverlayApp` (fire 202,
  zéro frame — observé par Keeper).
- **Non affecté** : le chemin scene-based (`PromoteStreamRule`, #154)
  porte un `Graph` plein issu de `Compile()` — `validateBlueprint()`
  (compile.go:723) y matérialise les data-nodes + defaults, puis
  `topologicalSort` ordonne. Seul le chemin blueprint-direct est cassé.

Cause racine architecturale : le promote blueprint-direct (ADR Blue 009
§3.3) a **réutilisé le seam simulate** parce qu'il compile « un blueprint
seul, sans scène porteuse » — mais le contrat du seam (exec-only,
zéro egress, data tranche vide) est un contrat de **dry-run**, pas un
contrat de **runtime**. Deux consommateurs aux besoins opposés partagent
un même entrypoint : c'est le défaut à corriger, pas l'authoring.

## 2. Decision drivers

1. Fix propre tranché par le porteur — pas de contournement authoring
   (forcer littéral/variable), qui laisserait ouverte toute la classe de
   drop silencieux.
2. **Zéro drift** entre les deux chemins de compile : les stream-rules
   scene-based et blueprint-direct doivent produire la même sémantique
   data pour un même graphe.
3. Le seam simulate d'ADR 015 garde son contrat (exec-only, **zéro
   egress** — pas de fetch manifest) : simulate reste un dry-run léger.
4. Invariant artefact byte-identique du push path scene (ADR 006 §3.1)
   intouché.
5. Le fix doit couvrir promote **et** reload (restart) — même machinerie.

## 3. Decision

**Go — option A de Conduit, sous la forme « entrypoint dédié »** : un
compile plein-graphe pour le blueprint-direct, qui **réutilise les
helpers exacts du chemin scene** au lieu d'étendre le seam simulate.

### 3.1 Nouvel entrypoint compilateur

`compiler.CompileBlueprintRule(ctx, bp, fetcher)` (nom final à Forge),
qui produit un `*Graph` **complet** pour un blueprint seul, clé `""` :

- **Data tranche** : `validateBlueprint(bp, manifest, execSet)` — le même
  helper que `Compile()` (compile.go:723) — puis `topologicalSort`.
  Matérialise **tous** les data-nodes (pas seulement le cône alimentant
  l'exec) : c'est la sémantique du chemin scene, et un pruning « cône »
  serait une divergence de plus à maintenir pour un gain nul (un data-node
  non consommé est inerte).
- **Exec tranche** : `partitionBlueprint` (inchangé, déjà partagé).
- **Defaults** : seeds de `validateBlueprint` (littéraux, ports non câblés)
  + `foldDeclaredVariables` (une seule fois — attention au double-fold,
  le seam actuel folde déjà les vars).
- **Bindings** : `platformStreamBindings`/`eventTopicBindings` dérivés des
  nodes du blueprint, pour qu'une rule blueprint-direct puisse consommer
  des inputs data-plane événementiels comme une scène (parité). Les
  adapters issus du **layout** (`extractAdapters`) n'existent pas ici —
  il n'y a pas de layout, c'est le seul écart légitime.
- **Manifest** : ce chemin a déjà l'egress (le promote fetch le blueprint
  via `deps.Fetcher`) ; `FetchComputeManifest` est disponible sur la même
  interface. Aucune extension d'interface requise.

### 3.2 Câblage API

`promoteBlueprintStreamRule` et `ReloadBlueprintStreamRules` passent sur
le nouvel entrypoint ; le `Graph` promu porte désormais `Nodes`,
`Bindings`, `Defaults`, `ExecPrograms`. `RenderBundle` reste vide (une
rule exécute, elle ne rend pas — inchangé).

### 3.3 Le seam simulate ne bouge pas

`CompileExecPrograms` (ADR 015) reste byte-identique dans son contrat et
ses artefacts : exec-only, zéro egress, `NO_EXEC_PROGRAM` fail-loud. Les
deux consommateurs sont désormais chacun sur l'entrypoint qui porte leur
contrat.

### Alternatives écartées

- **Étendre `CompileExecPrograms` pour matérialiser le data plane** :
  casse le contrat simulate (le fetch manifest = egress, interdit par
  ADR 015) et fait diverger les artefacts simulate ; on garderait un
  seul entrypoint pour deux contrats opposés — la cause racine.
- **Scène porteuse synthétique via `Compile()`** : sémantiquement faux
  (« a blueprint is a blueprint, not a scene » — doctrine du promote),
  traîne fetch canvas/layout/render pour rien, exige un CanvasVersion
  fictif.
- **Contournement authoring** (folder `app_id` en littéral/variable) :
  rejeté par le porteur ; ne ferme pas la classe de drop silencieux —
  chaque futur blueprint-direct retomberait dans le piège.
- **Pruning au cône data des exec-inputs** : divergence sémantique
  supplémentaire vs scene path, complexité de graphe pour un gain nul.

## 4. Consequences

- Toute forme d'authoring data-plane devient valide dans une stream-rule
  blueprint-direct — parité complète avec les rules scene-based ; le
  Marker (`bp-overlay-app-marker-test`) résout `app_id` par edge câblé.
- Le promote blueprint-direct gagne un fetch manifest (egress +1 appel,
  amorti HTTP keepalive) — coût négligeable, promote est une opération
  opérateur rare.
- Les blueprints-direct déjà promus/persistés sont recompilés au prochain
  reload avec la nouvelle machinerie (le store persiste `blueprint_id`,
  pas l'artefact — pas de migration).
- Un blueprint-direct **pure-dataflow** (zéro exec) reste rejeté au
  promote (une rule sans entrypoint exec n'a pas de sens) — comportement
  à préserver explicitement, équivalent `NO_EXEC_PROGRAM` côté nouvel
  entrypoint.

## 5. Risks

- **Drift d'orchestration** : le nouvel entrypoint duplique
  l'orchestration de `Compile()` (pas ses helpers). Mitigation : tests de
  parité — même blueprint compilé via scene-path et via blueprint-direct
  ⇒ mêmes GraphNodes/Defaults (modulo préfixe de clé et artefacts layout).
- **Régression simulate** : couverte par les tests contrats ADR 015
  existants (aucun changement du seam — vérifiable par diff nul sur
  `compile_exec_inbody.go`).
- **Double-fold des variables déclarées** (§3.1) : à tester explicitement
  (une variable déclarée ne doit seeder qu'une fois).
- **Surface review** : compilateur core + runtime graph, **aucune surface
  auth/secrets/réseau nouvelle** (l'egress manifest passe par le Fetcher
  gateway existant, même trust). **Bastion non requis** ; validation ADR
  par Vigil, merge par Eleven sur CI verte (+ Vigil si rouge).

## 6. Resolution criteria

1. Un blueprint-direct dont un input exec est alimenté par un data-node
   câblé (edge normal, non littéral/variable/payload) résout la valeur au
   runtime — test unitaire compiler + test runtime (promote → fire →
   valeur observée).
2. Parité scene/blueprint-direct : test comparant les tranches data
   compilées par les deux chemins pour un même blueprint.
3. Marker end-to-end : `bp-overlay-app-marker-test` promu, toggle on-call
   → `EmitOverlayApp` émet des frames sur le wire (harnais Keeper) — le
   « fire 202, zéro frame » disparaît.
4. Seam simulate inchangé : suite ADR 015 verte, artefacts byte-identiques.
5. Push path scene byte-identique : suites existantes vertes sans
   modification des artefacts.
6. Reload (restart) recompile via le même entrypoint — test couvrant
   `ReloadBlueprintStreamRules`.
7. Promote d'un blueprint pure-dataflow toujours rejeté fail-loud.
