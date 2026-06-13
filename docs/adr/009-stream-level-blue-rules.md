# ADR 009 — Stream-level Blue rules (show-scope execution)

- **Status**: accepted
- **Date**: 2026-06-13
- **Decided**: 2026-06-13
- **Deciders**: @ClodoCapeo
- **Author**: Atlas
- **Supersedes**: — (complète ADR 008 ; ne renverse rien)
- **Superseded by**: —

## 1. Context

ADR 008 (accepté, implémenté #152) : seule la scène active exécute — inbox
(`adapters/inbox.go:138`) et tick (`runtime/tick.go:86`) routent vers
`show.Active()` seul ; le roster est un backstage gelé (freeze-resume par
scène, dataflow gate `scene.go:645` en couche 2).

Exigence porteur (2026-06-12, complément explicite) : des **règles Blue
stream-level** — indépendantes des scènes, **toujours actives** quelle que
soit la scène à l'antenne (« règles blues activées sur toutes les scènes…
stream level, pratique pour Quasar »). Cas d'usage : réagir aux events
plateforme (follow/sub/raid/chat) sur n'importe quelle scène, état persistant
à travers les switches (compteurs de show), modération/actions — sans
dupliquer le handler dans chaque scène ni perdre l'état au switch (le
freeze-resume d'ADR 008 est par-scène, pas global). Terrain naturel de
**Cosmos** (ADR 001 Quasar) côté consommation.

Faits de code pertinents :

- `Show` (`runtime/show.go`) : `scenes map[string]*Scene` + `active` ;
  `SetActive` (show.go:271) ne touche que sortante/destination.
- Une `Scene` est déjà l'unité d'exécution complète : state isolé par
  instance, exec programs (R9 : installés ssi version validée, `LoadExec`
  show.go:159), acceptance déclarée (`sceneAcceptsPath`, inbox.go:195),
  bindings synthétiques `platform-stream` (compile.go:917) et `event-topic`
  (#148).
- Le pipeline push/validate (`api/scenes_push.go`, `execForAir`) est le seul
  chemin qui arme des effets-monde — gate R9 non négociable.
- Inbox sans scène active : write **absorbé** (inbox.go:139-148) — les events
  Quasar entre deux scènes sont perdus aujourd'hui.

## 2. Decision drivers

1. Doctrine porteur : stream-level = toujours vivant ; ADR 008 reste vrai
   pour le roster (dormant = zéro logique). Le set d'exécution devient
   **{active} ∪ {stream-rules}** — complément, pas renversement.
2. Ne pas réinventer l'unité d'exécution : la `Scene` (state, exec, R9,
   acceptance) est prouvée live — la réutiliser telle quelle.
3. Zéro changement Canvas/Blue si possible (le langage et l'authoring
   existants suffisent — doctrine `orion-must-serve-all-of-blue`).
4. Pas de fan-out resurgi : la délivrance multiple est **bornée par une
   activation opérateur explicite**, pas par « tout le roster ».
5. Invariant de switch prouvé live intact ; `SetActive` intouché.
6. Surface d'auth inchangée (`CanWritePath`, rôles, gate R9).

## 3. Decision

### 3.1 Forme authorée : une scène logic-only, promue « stream rule » à l'activation

Une règle stream-level **est une scène Orion ordinaire** : blueprint(s) Blue +
layout Canvas minimal (root vide), poussée par le pipeline standard
(`POST /scenes/{id}/push`), **validée** (gate R9 inchangé — une règle exécute
des effets-monde, elle passe la même harness). Aucun flag compilateur, aucun
nouveau format : la promotion en règle est un **acte d'activation runtime**,
pas une propriété compilée.

- Alternatives écartées :
  - *Blueprint show-level dédié (nouvelle entité, nouveau push)* : duplique
    tout le pipeline (compile, persist, versionning, validation R9, rollback)
    pour un artefact identique à 95 % — coût sans gain ; et fige côté
    Blue/Canvas une distinction qui n'est qu'un mode d'exécution.
  - *« Scène stream » spéciale toujours active (2e pointeur actif)* : un seul
    slot — or le porteur veut des **règles** (pluriel, composables :
    alertes + compteur + modération activables indépendamment).
  - *Registre de règles bespoke côté `Show` (micro-runtime)* : réinvente
    state/exec/acceptance hors de la `Scene` — interdit par driver 2.
- Le bundle de rendu d'une règle est **ignoré au runtime v1** (§ 3.5) ; la
  convention d'authoring est layout vide. Pas de reject compilateur (une
  scène normale doit pouvoir être promue/dépromue sans re-push).

### 3.2 Activation, stockage, API

`Show` gagne un second set : `streamRules map[string]*Scene`, disjoint du
roster actif. API (operator-gated, `requireOperator`, même modèle que
`POST /show/active-scene`) :

- `POST /api/v1/show/stream-rules` `{scene_id}` — promeut une scène poussée
  **et validée** (réutilise `execForAir` ; refus `SCENE_NOT_VALIDATED` sinon
  — une règle sans exec n'a pas de raison d'être) ; reject si la scène est
  l'active (`RULE_IS_ACTIVE_SCENE`).
- `DELETE /api/v1/show/stream-rules/{scene_id}` — dépromotion :
  `CancelExec` + retrait du set (l'instance roster, si chargée, reste
  dormante normale).
- `GET /api/v1/show` expose le set (`stream_rules: [...]`).

Persistance : table `show_stream_rules(scene_id)` à côté de
`show_state.active_scene_id` ; reload au boot (`loadActiveScenes` étendu).
Garde-fous croisés : `POST /show/active-scene` refuse une scène actuellement
règle (même code) ; archive refuse une règle active (`SCENE_IN_USE`,
extension du guard existant). Re-push d'une scène promue : `LoadExec` détecte
l'appartenance au set règles et swap **l'instance règle** (restart-reseed,
mêmes sémantiques §3.1.4 ADR 003) — l'antenne n'émet rien.

### 3.3 Routage : union {active} ∪ {stream-rules} aux deux sites

`Show` expose `RouteTargets() []*Scene` — lu sous un seul `RLock` : l'active
(si non-nil) puis les règles triées par id (ordre déterministe). Les deux
producteurs l'utilisent :

- **Inbox** (`inbox.go:138`) : pour chaque cible, `sceneAcceptsPath` décide
  individuellement ; délivrance à chaque cible qui déclare le path. Un seul
  enregistrement d'audit par write (inchangé). Drop metric par instance
  (`orion_inbox_dropped_total{scene_id}` couvre déjà).
- **Tick** (`tick.go:86`) : même union — une règle on-tick vit en continu.

Conséquences voulues : (a) un write `__inputs.platform.*` de Quasar atteint
l'active **et** chaque règle qui déclare le binding ; (b) **sans scène active**
(`active == ""`), les règles reçoivent quand même — les events plateforme
entre deux scènes ne sont plus perdus pour les règles (l'absorption côté
antenne demeure). Linearisation : même contrat qu'ADR 008 §3.1 (snapshot du
set au moment de la lecture ; un write en vol pendant une (dé)promotion va au
set d'alors — accepté, events live-only).

- Alternative écartée : *router les règles via un canal séparé de l'inbox*.
  Deux points d'audit, deux gates d'acceptance, divergence garantie. L'inbox
  reste le point d'audit unique (ADR 004 § 5).

Ce n'est **pas** le fan-out d'ADR 004 § 5 rule 4 ressuscité : la délivrance
multiple est bornée au set explicitement promu par l'opérateur (typiquement
0–3 règles), pas au roster entier ; le roster reste dormant par non-routage.

### 3.4 État : jamais gelé, isolé par instance, pont par événements

- Une règle n'apparaît **jamais** dans `SetActive` : jamais de `CancelExec`
  au switch, jamais de `SetOnAir(false)`, jamais de freeze. Son instance est
  marquée non-gatée à la promotion (équivalent `SeedOnAir(true)` permanent ;
  le dataflow gate `scene.go:645` ne la bloque jamais). `FireOnStart` fire
  **une fois à la promotion** (et au boot-reload), pas à chaque switch.
- **Pas de namespace `__vars` show-level partagé.** L'état d'une règle vit
  dans son instance `Scene` (isolation par construction, comme toute scène) —
  ses `__vars` sont rule-local et survivent aux switches parce que rien ne
  les gèle. Aucune lecture croisée règle↔scène : un état partagé mutable
  show-level créerait des écritures concurrentes inter-goroutines sur le
  modèle single-writer. La communication règle→antenne passe par § 3.6.
- Restart process : reseed defaults (criterion #11 ADR 004 réaffirmé — aucun
  état live persisté) ; une règle qui veut un compteur durable le persiste
  via `db.query` (pattern canonique).

### 3.5 Rendu : pure-logique en v1, overlay cadré pour plus tard

**v1 : les règles sont pure-logique.** Pas de subscriber attachable, pas de
mirror LSDP (`SetMirror` jamais appelé sur une instance règle), bundle ignoré.
Le rendu reste le monopole de la scène active (Solar inchangé). Une règle
*influence* l'antenne via § 3.6 : elle émet un événement que la scène active
rend si elle embarque le composant écouteur (composant Canvas réutilisable —
la déduplication de la *logique* est totale, celle du *rendu* passe par la
réutilisation de composants, mécanisme d'authoring existant).

**v2 (cadré, non décidé ici)** : overlay persistant composité par-dessus
toute scène (compteur d'abonnés permanent). Exige une couche de composition
Solar (multi-source LSDP ou seconde browser-source Pulsar) et une sémantique
`scene_changed` multi-flux — chantier transverse Solar/Pulsar/LSDP qui fera
l'objet de son propre ADR quand le besoin visuel sera réel. Rien dans v1 ne
l'hypothèque (le bundle d'une règle est déjà compilé et persisté).

### 3.6 Pont règle→antenne : primitive `show.emit@1` (82e primitive Blue)

`show.emit@1` (config : `topic`, input : `payload`) est un **vrai primitive
authored** — le 82e du stdlib Blue — pas un simple op runtime enregistré côté
Orion. Le gate CI `exec-port-parity`
(`internal/conformance/signature_parity_test.go:51-119`) est
**bidirectionnel** : tout op servi par le runtime doit exister dans la seed
signature Blue (`signatures.json` généré par `stdlib_seeder.py`), et
réciproquement. Servir `show.emit@1` exige donc son périmètre Blue :

- **seed Blue** (`stdlib_seeder.py`) : signature `show.emit@1` — config
  `topic`, input `payload`, sémantique (émission d'un événement de show
  `__events.<topic>` vers la scène active) ;
- régénération `signatures.json` + `manifest.json` ;
- passage du gate `exec-port-parity` + enregistrement dans la conformance
  matrix côté Orion (criterion #1/#2 ADR 004).

La frontière est nette : **le mécanisme stream-rule (§3.1–3.4) = zéro
changement Blue ; le seul ajout Blue de cet ADR est le primitive
`show.emit@1`** — un ajout de stdlib ordinaire, comme tout primitive.

**Sémantique runtime — chemin d'injection système, active-only.**
L'exécuteur injecte un write système `__events.<topic>` délivré à la **scène
active seule** (`show.Active()`), via le chemin inbox système (audité). Ce
chemin est un **nouveau chemin runtime distinct de l'union § 3.3** — il ne
passe **pas** par `RouteTargets()` : s'il suivait l'union, l'émission d'une
règle cascaderait vers les autres règles (boucles). L'asymétrie anti-loop est
normative :

- **emit règle→active** : hors union — cible `show.Active()` seul ; jamais
  règle → règle, pas de cascade possible par construction ;
- **write `__events.*` du wire** (operator/service, ex. Cosmos) : suit le
  routage union § 3.3 — origine externe, pas de boucle.

Disponible pour toute scène (doctrine full-Blue — pas réservé aux règles ;
depuis l'active il est simplement réflexif… et no-op utile : il refire ses
propres entrées on-event, à documenter comme tel).

### 3.7 Quasar / Cosmos

Les bindings `platform-stream` et `event-topic` d'une règle sont synthétisés
au compile exactement comme pour une scène — aucun changement compilateur
pour le **mécanisme** de règles ; le seul ajout chaîne-compile de cet ADR est
la signature `show.emit@1` (§ 3.6), absorbée comme tout nouveau primitive
(manifest régénéré) :
une règle **déclare** les paths qu'elle accepte par les nœuds qu'elle authore.
`CanWritePath` inchangé ; le service token Quasar garde son scope
`__inputs.platform.*`. Cosmos, quand il existera, pourra fire des
`__events.*` vers règles+antenne avec un token scopé `__events.<préfixe>` —
ce minting-là passera par Bastion (vigilance ADR 008 § 5 réaffirmée).

## 4. Consequences

- Le modèle mental devient : « une scène vit à l'antenne ; des règles de
  show vivent en permanence à côté ; le roster reste un backstage gelé ».
  ADR 008 intact pour le roster.
- L'absorption « pas de scène active » ne vaut plus pour les règles — un
  show sans antenne garde sa logique de fond.
- Coût runtime borné : N règles promues = N goroutines déjà existantes
  (instances chargées) qui reçoivent désormais des inputs ; recompute
  dirty-driven inchangé.
- Canvas, Solar, Quasar : **zéro changement**. **Blue : le mécanisme
  stream-rule (promotion, routage, état, acceptance) = zéro changement ; le
  seul ajout est le primitive `show.emit@1`** (seed `stdlib_seeder.py` +
  régénération `signatures.json`/`manifest.json`, § 3.6) — exigé par le gate
  bidirectionnel `exec-port-parity`. Prism : affordance UI de
  promotion/dépromotion (follow-up, hors périmètre Orion).
- `LoadExec`/`Unload`/archive gagnent la conscience du set règles (§ 3.2).

## 5. Risks

- **R1 — règle bavarde** : une règle on-tick + `show.emit` à 60 Hz spamme
  les entrées on-event de l'active. Mitigation : c'est l'équivalent exact
  d'un on-tick authoré dans la scène (budget exec/shed existant, métriques
  #82) ; pas de throttle dédié en v1, métrique d'émission ajoutée.
- **R2 — écart authoring** : rien ne distingue visuellement une « scène » d'une
  « règle » à l'authoring ; risque de promouvoir une scène visuelle par
  erreur (son rendu serait silencieusement ignoré). Mitigation : warning API
  à la promotion si le bundle est non-trivial + affordance Prism.
- **R3 — réintroduction rampante du fan-out** : un futur contributeur étend
  `RouteTargets` au roster. Mitigation : test négatif (criterion 1 ADR 008
  conservé) + commentaire normatif au site.
- **Sécurité — surface inchangée en v1** : promotion operator-gated (mêmes
  rôles que `active-scene`), exec gated R9 (une règle non validée ne
  s'active pas), `CanWritePath` et scopes intacts, aucun nouveau endpoint
  public, `show.emit` est un artefact authored+validé (même modèle de
  confiance que les effets existants). **Pas de clearance Bastion requise
  pour cet ADR.** Points de vigilance futurs (Bastion obligatoire le jour
  venu) : (a) token service scopé `__events.*` pour Cosmos ; (b) toute
  promotion de règle déclenchée par un service (non-operator).

## 6. Resolution criteria (testables)

1. **Union de routage** : un write accepté est délivré à l'active **et** à
   chaque règle promue déclarant le path ; une scène du roster non-promue et
   non-active ne reçoit toujours rien (criterion 1 ADR 008 reste vert,
   assertions inchangées).
2. **Toujours vivant** : une règle on-tick fire à travers un switch A→B et
   pendant `active == ""` ; un write `__inputs.platform.*` atteint la règle
   sans scène active.
3. **État persistant** : un compteur `__vars` de règle incrémenté sur N
   events survit à A→B→A sans freeze ; `CancelExec` n'est jamais invoqué sur
   la règle au switch (assert zéro cancel).
4. **show.emit** : une règle émettant `__events.alert` fire l'entrée
   on-event de la scène active ; aucune autre règle écoutant `alert` ne fire ;
   l'événement apparaît dans l'audit ring.
5. **Lifecycle** : promotion refuse non-poussée / non-validée
   (`SCENE_NOT_VALIDATED`) et l'active (`RULE_IS_ACTIVE_SCENE`) ;
   `active-scene` refuse une règle ; archive d'une règle promue →
   `SCENE_IN_USE` ; le set survit au restart (reload boot) ; dépromotion
   cancel les tâches vives.
6. **Re-push d'une règle promue** : swap de l'instance règle
   (restart-reseed), aucune émission `scene_changed`/snapshot sur l'antenne.
7. **Pas de rendu v1** : aucune voie d'attache subscriber/mirror sur une
   instance règle ; la suite d'invariant de switch existante reste verte
   sans modification.
8. **Conformance & parity** : `show.emit@1` a un executor enregistré + test
   dans la matrix, est seedé côté Blue (`signatures.json`/`manifest.json`
   régénérés) et le gate `exec-port-parity` passe — les fixtures
   conformance/signatures **changent** (ajout du 82e primitive, attendu).
   En revanche, le hash `scene_version` et le render-bundle de toute scène
   existante restent **stables** : le mécanisme stream-rule n'introduit
   aucun changement compilateur, et une signature ajoutée mais non utilisée
   par une scène ne modifie pas son hash.
