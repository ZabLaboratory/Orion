# ADR 013 — Platform-event exec entrypoint (`core.event.on-platform-event@1`)

- **Status**: accepted
- **Date**: 2026-06-13
- **Decided**: 2026-06-13
- **Deciders**: @ClodoCapeo
- **Author**: Atlas
- **Supersedes**: — (étend ADR 008 §3.3 + ADR 009 §3.6 ; ne renverse rien)
- **Superseded by**: —

## 1. Context

La finale de la campagne de validation Blue veut prouver le pont complet :
**event Twitch réel → règle stream-level Blue (toujours active, ADR 009) →
`core.show.emit@1` → la scène active réagit**. Ce pont ne compile pas en un
chemin qui *fire* aujourd'hui — gap moteur réel, vérifié sur `main` :

- `core.show.emit@1` est un nœud **exec** (in `in` / out `then`,
  `Blue/src/blue/services/stdlib_seeder.py:1255-1294`). Il lui faut un **point
  d'entrée exec** (une `ExecEntry`) en amont de sa spine.
- Le vocabulaire d'arming exec est **fermé à 3 kinds** : `on-start` / `on-tick`
  / `on-event` (`internal/runtime/exec.go:96-98`, mirror compilateur
  `internal/compiler/exec_partition.go:55-59` → `core.event.{on-start,on-tick,
  on-event}@1`).
- `on-event` ne fire **que** sur un write d'un path préfixé `__events.`
  (`internal/runtime/scene.go:688`, `eventsPrefix = "__events."`). Le
  compilateur force ce préfixe sur le `event_name` (`compile.go` event-topic
  bindings, `eventsLeafPrefix`).
- Quasar écrit `__inputs.platform.twitch.<channel>.last_<type>` — un leaf
  **dataflow pur** : les 14 `quasar.twitch.*@1` sont des `Kind:"input"` sans
  `ExecProgram` (`inbox_platform_test.go:28-44`). Acceptés par
  `platformStreamBindings` (`compile.go:1036`, `Kind:"platform-stream"`), pas
  par `eventTopicBindings` (`Kind:"event-topic"`).

**Conséquence** : un event chat réel déclenche le recompute **dataflow** (M9/M1
prouvé live) mais **n'arme aucune entrée exec**. Une règle authorée en
`on-event "__inputs.platform.twitch…"` *compile* (un binding synthétisé sur
`__events.__inputs.platform…`) mais **ne fire jamais** : Quasar n'écrit pas dans
le namespace `__events.`. Donc `show.emit` dans une règle stream-level est
inatteignable depuis un event plateforme. C'est exactement le maillon qui manque
pour la finale.

Briques déjà en place qu'on réutilise :

- ADR 009 `Inbox.EmitToActive` (`inbox.go:203`) : un write `__events.<topic>`
  arme déjà les `on-event` de la **scène active seule**, sans cascade rule→rule.
  C'est le précédent exact d'un write-arme-une-spine, audité, active-only.
- ADR 008 `eventTopicBindings` (`compile.go:1077`) : miroir d'acceptance pour
  les topics `__events.*`.
- `platformStreamBindings` : acceptance des leaves `__inputs.platform.*`
  (service-token Quasar, paths) — déjà routée vers la scène/règle.
- Routage union ADR 009 : `{active} ∪ {stream-rules}`, `PromoteStreamRule`,
  scope d'exécution stream-level — **déjà là**, c'est le scope dans lequel la
  règle de la finale tournera.

## 2. Decision drivers

- **Esprit M9/M1 / doctrine porteur** : event-driven prouvé live ; le polling est
  un anti-pattern explicite (`live-testing.md`, [[stream-level-blue-rules]]).
- **[[orion-must-serve-all-of-blue]]** : ne jamais restreindre le langage servi.
  La sûreté est un gate d'authoring, pas une limitation moteur.
- **Coût Orion** (runtime + compilo) le plus bas pour le pont — on est sur Fable.
- **Parité seed↔runtime** (gate `conformance-matrix`, exec-port-parity,
  round-trip `exec_partition`) : tout nouveau primitive exec doit avoir executor
  + entrée mappée + test, sinon `EXEC_OP_UNMAPPED`.
- **Coexistence** : les 14 `quasar.twitch.*@1` (dataflow) restent authorables et
  intacts. Le réactif dataflow M9 ne doit pas régresser.
- **Pas de double-fire / pas de boucle** : un event = un fire ; pas de nouvelle
  sémantique runtime de propagation.

## 3. Decision

**Option 1, variante « nouveau primitive exec paramétré par path » :** ajouter un
entrypoint exec **`core.event.on-platform-event@1`** dont l'arming se branche sur
le **write du leaf `__inputs.platform.*` lui-même** — pas sur `__events.*`.

Mécanique, en réutilisant au maximum l'existant :

1. **Runtime — nouveau kind `on-platform-event`** (`exec.go`). Une `ExecEntry`
   de ce kind porte, dans son champ existant `Event`, le **leaf plateforme
   complet** qu'elle observe (`__inputs.platform.twitch.<channel>.last_chat`),
   et expose ses data-out pins (`<node>.value`, `<node>.user`, … selon le type
   d'event) sous son namespace `Node` — exactement comme `on-tick` lie
   `<node>.delta_seconds`.

2. **Runtime — firing hook** (`scene.go`, à côté du bloc `on-event`/`on-tick`,
   ~ligne 688). Indexer ces entries dans une map `execOnPlatform[leafPath]` et,
   sur un write dont `msg.Path` a le préfixe `platformLeafPrefix`
   (`__inputs.platform.`), `enqueueFire` les entries qui observent ce leaf.
   Le write dataflow continue de s'appliquer **avant** (le recompute M9 reste
   intact) ; le fire exec s'ajoute **après**, sur le même write, sans
   re-déclenchement (« fire on the WRITE, not on the value change » — invariant
   `scene.go:672`). **Un write = un fire**, pas de double-fire.

3. **Compilateur** (`exec_partition.go`) : mapper `core.event.on-platform-event@1`
   → kind `on-platform-event` dans `execEntryKind` ; le `event` de l'entrée =
   le leaf plateforme canonicalisé via `platformLeafPath` (casefold-then-validate
   du channel, **réutilisé tel quel** — même charset, mêmes diagnostics
   `PLATFORM_CHANNEL_INVALID`). **Pas** de préfixe `__events.` ici : le path
   reste dans le namespace `__inputs.platform.`.

4. **Compilateur — acceptance** : le leaf observé par une entrée
   `on-platform-event` rejoint le set traité par `platformStreamBindings`
   (réutilisé), pas par `eventTopicBindings`. `sceneAcceptsPath` accepte donc
   déjà le write Quasar — **aucun nouveau binding Kind, aucune nouvelle surface
   d'acceptance** ; on étend juste la collecte des leaves à inclure ceux portés
   par une entrée exec en plus de ceux portés par un nœud `input`.

5. **Scope d'exécution** : aucune nouveauté. Une règle stream-level (ADR 009)
   qui contient une entrée `on-platform-event` est promue (`PromoteStreamRule`)
   et tourne dans l'union `{active} ∪ {stream-rules}` ; son `show.emit` atteint
   la scène active via `EmitToActive` **inchangé**. Le routage active-only
   (ADR 008) et le freeze-resume restent tels quels : une scène dormante ne
   reçoit aucun write plateforme (gate dataflow `scene.go:645`), donc ne fire
   pas — cohérent avec l'invariant active-scene-only.

6. **Blue seed — nouveau primitive exec, coexistant** : un nœud
   `core.event.on-platform-event@1` exec-bearing (out `then` + data-out pins du
   payload), **paramétré** par `platform` + `channel` + `event_type` (config).
   Les 14 `quasar.twitch.*@1` **dataflow restent inchangés et authorables** :
   ils servent le câblage réactif M9 (sélection/affichage), le nouveau primitive
   sert l'**arming d'une spine**. Les deux écrivent/observent le **même leaf
   canonique** `__inputs.platform.*` — un blueprint peut très bien avoir le
   `quasar.twitch.chat@1` dataflow (pour lire la valeur) ET un
   `on-platform-event` (pour armer), tous deux sur le même channel.

Cette variante est préférée à « généraliser `on-event` pour s'armer sur
`__inputs.platform.*` » parce que : (a) elle garde `on-event` mono-sémantique
(`__events.` reste le namespace d'émission opérateur/règle, ADR 009 — ne pas le
polluer avec le namespace d'ingestion plateforme) ; (b) un primitive dédié est
**authorable et lisible** côté Blue/Prism (l'auteur choisit explicitement « armer
sur un event plateforme ») et porte une config typée `platform/channel/event_type`
au lieu d'un path brut ; (c) le diagnostic `platformLeafPath` est réutilisé tel
quel pour la validation du channel.

## 4. Consequences

**Orion runtime** : un kind exec de plus, un index `execOnPlatform` + une branche
de firing dans `scene.go` (symétrique du bloc `on-event`). Aucune nouvelle
sémantique de propagation (pas de pont dataflow→exec générique — on évite
explicitement l'option 2 et son risque de boucle). `validation_harness` /
`exec_validation` : ajouter le kind à la couverture.

**Orion compilateur** : une entrée de plus dans `execEntryKind` ; extension de la
collecte de leaves plateforme pour `platformStreamBindings` afin d'inclure les
leaves portés par une entrée exec. Round-trip `exec_partition_roundtrip_test`
mis à jour (nouveau kind sérialisé). Hash `scene_version` déterministe préservé
(leaves triés).

**Blue seed** : un primitive exec de plus dans le stdlib seeder. Les 14 inputs
dataflow inchangés → zéro régression M9. La parité signature seed↔runtime
(`signature_parity_test`, `conformance.go`) couvre le nouveau primitive : in/out
pins + data-out du payload.

**Parité / conformance** : `conformance-matrix` exige executor + entry mappée +
test pour tout manifest node servi. Le nouveau kind doit être dans la matrice
sinon `EXEC_OP_UNMAPPED` au push. Exec-port-parity : les data-out pins exposés
runtime = ceux déclarés seed.

**Pas de régression de coexistence** : `inbox_platform_test` (dataflow) reste
vert tel quel ; un nouveau test prouve qu'un write plateforme arme **aussi** une
entrée exec quand une est présente.

## 5. Risks

- **Double-fire** si un blueprint a deux entrées `on-platform-event` sur le même
  leaf : comportement attendu (deux entrées = deux spines), pas un bug — mais à
  documenter. Mitigé : on fire par `enqueueFire`, une tâche par entrée, comme
  `on-event`.
- **Surface** : réception pure (un write Quasar arme une spine locale). Aucune
  sortie réseau, aucune modération sortante, aucun nouveau secret/scope (le
  service-token Quasar et `platformStreamBindings` couvrent déjà le namespace).
  **Pas de surface d'attaque nouvelle → Bastion non requis.** Signalé pour
  traçabilité ; si la finale ajoutait une action *sortante* (ban/timeout via
  Quasar), ce serait une autre ADR avec clearance Bastion.
- **Mauvais channel/type** : capté à l'authoring par `platformLeafPath`
  (`PLATFORM_CHANNEL_INVALID`) — pas un risque runtime.
- **Boucle rule→rule** : exclue par construction — `EmitToActive` ne fan-out pas
  vers les règles (ADR 009 §3.6), et `on-platform-event` ne s'arme que sur
  `__inputs.platform.*`, jamais sur `__events.*`. Aucun chemin event→règle→event.

## 6. Resolution criteria (testables)

1. **Compilo — mapping & acceptance** : un blueprint avec un
   `core.event.on-platform-event@1` (twitch/`<channel>`/`chat`) compile en une
   `ExecEntry{Kind:"on-platform-event", Event:"__inputs.platform.twitch.<channel>.last_chat"}`
   ET produit un `platformStreamBindings` couvrant ce leaf. Channel invalide →
   `PLATFORM_CHANNEL_INVALID` au push. (test compilateur)
2. **Round-trip** : `exec_partition_roundtrip_test` re-décode le nouveau kind
   byte-identique via `runtime.ExecProgramsFromGraph`.
3. **Runtime — fire** : un write `__inputs.platform.twitch.<channel>.last_chat`
   dans la scène (ou règle) qui porte l'entrée fire la spine **exactement une
   fois** par write, avec les data-out du payload liés sous le node namespace.
   (test runtime, à côté de `exec_show_emit_test`)
4. **Coexistence dataflow** : `inbox_platform_test` reste vert ; un blueprint
   portant à la fois le `quasar.twitch.chat@1` dataflow et un `on-platform-event`
   sur le même channel : le dataflow recompute ET l'exec fire, sur le même write.
5. **Stream-level end-to-end (Orion)** : une règle promue (ADR 009) contenant
   `on-platform-event → show.emit@1` : un write plateforme dans l'inbox fire la
   règle, `EmitToActive` injecte `__events.<topic>` dans la scène active, dont
   l'`on-event` fire. Audité une fois. Scène dormante non armée.
6. **Parité seed↔runtime** : `signature_parity_test` + `conformance-matrix`
   verts avec le nouveau primitive (executor + entry + data-out pins déclarés).
7. **Conformance** : `TestConformance_Matrix` (un nœud de chaque type servi)
   inclut `core.event.on-platform-event@1` sans `EXEC_OP_UNMAPPED`.
8. **Live (finale, critère porteur)** : un message chat Twitch réel sur le
   channel live → règle stream-level → `show.emit` → réaction visible **à
   l'antenne** sur la scène active, prouvé par record `.mp4` (`live-testing.md`,
   pas un snapshot wire). C'est le seul critère de « fini » de la campagne.

CI verte + deploy vert (`GET /orion/api/v1/health` via ZabGate) restent le gate
de merge standard (`CLAUDE.md` Orion).
