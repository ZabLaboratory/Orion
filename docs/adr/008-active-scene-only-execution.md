# ADR 008 — Active-scene-only execution (dormant roster)

- **Status**: accepted
- **Date**: 2026-06-12
- **Decided**: 2026-06-13
- **Deciders**: @ClodoCapeo
- **Author**: Atlas
- **Supersedes**: ADR 004 § 5 rule 4 (inbox fan-out à toutes les scènes chargées) — partiellement ; étend ADR 006 § 3.4 (air-only trigger scope)
- **Superseded by**: —

## 1. Context

Doctrine porteur (campagne de validation des primitives, 2026-06-12) : **seule la
scène active exécute ses blues**. Les scènes chargées non-actives sont
**dormantes** — pas d'on-events, pas de tick, pas de logique (dataflow compris).

État du code (source de vérité) :

- L'inbox fan-out chaque write accepté à **toutes** les scènes chargées qui
  déclarent le path (`internal/adapters/inbox.go:128`, règle ADR 004 § 5 rule 4).
  Le tick `__system.*` fan-out de même (`sceneAcceptsPath`, inbox.go:199).
- `Show` (`internal/runtime/show.go`) garde `scenes map[string]*Scene` + pointeur
  `active`. `SetActive` (show.go:263) migre les subscribers, émet
  `scene_changed` + snapshot frais (invariant de switch **prouvé live**,
  `pulsar-scene-agnostic-single-live-invariant`), `CancelExec` + `SetOnAir(false)`
  sur la précédente, `SetOnAir(true)` + `FireOnStart` sur la destination.
- ADR 006 § 3.4 a déjà gaté le **firing des triggers exec** (on-tick/on-event)
  sur le flag `onAir` (`scene.go:649`). Mais : (a) le **dataflow** d'une scène
  off-air recompute toujours (chaque write fan-outé met à jour son state et
  déclenche son cône de recalcul) ; (b) le flag ne gouverne que le firing, pas le
  **routage** — chaque scène chargée reçoit et traite chaque write.
- Gap on-event : un write `__events.<topic>` est droppé par `sceneAcceptsPath`
  (inbox.go:177) — le compilateur n'enregistre l'`event_name` d'une entrée
  on-event (`exec_partition.go:66`, clé `event_name`) dans **aucune** des trois
  sources d'acceptance (`Defaults` / `OperatorInputs` / `Bindings.TargetPaths`).
  Le précédent exact pour déclarer un path d'acceptance synthétique existe :
  `platformStreamBindings` (`compile.go:888`) inscrit les leaves
  `__inputs.platform.*` comme `ExternalAdapter{Kind:"platform-stream"}` pur
  (aucune goroutine, pure déclaration d'acceptance).

`active` ne gouverne donc aujourd'hui que le **rendu** (et les triggers exec
depuis ADR 006), pas l'**exécution** au sens plein. Le porteur refuse ce modèle.

## 2. Decision drivers

1. Doctrine porteur : dormant = zéro logique (exec **et** dataflow).
2. Ne pas casser l'invariant de switch prouvé live (A→B→C, `scene_changed` +
   snapshot ≤ 100 ms, migration des subscribers, Solar re-render).
3. Subsumer le fix on-event sans réintroduire le fan-out (l'option « auto-déclarer
   les topics » seule ferait fire toutes les scènes chargées).
4. Surface d'auth inchangée : `CanWritePath` (`auth/identity.go:82`) reste le
   garde role/scope ; l'acceptance déclarée par la scène reste le second étage.
5. Conserver le switch instantané : les scènes restent **pré-construites et
   chargées** (snapshot prêt) — dormant ≠ déchargé.

## 3. Decision

### 3.1 Gating de l'exécution : routage à l'inbox, scène active uniquement

Le fan-out de `Inbox.Write` (inbox.go:128) est remplacé par un **routage vers la
seule scène active** (`show.Active()`). Idem pour le tick `__system.*` : il
n'atteint que la scène active. `sceneAcceptsPath` reste l'acceptance gate, évalué
sur la scène active seule ; un write que l'active ne déclare pas est absorbé
(comportement actuel pour une scène non-déclarante, inchangé).

- **Mécanisme retenu : routage, pas suspension de goroutine.** La boucle
  `scene.Run` de chaque scène chargée reste vivante mais **quiescente** : un
  goroutine bloqué sur un channel vide coûte zéro CPU. Aucun write ne lui
  parvient → aucun recompute, aucun trigger, aucune logique. Dormance garantie
  par construction (pas d'input), avec le gate `onAir` d'ADR 006 conservé en
  **défense en profondeur** (couche 2 : si un message interne atteignait une
  scène off-air, ses triggers ne fireraient toujours pas).
- Alternatives écartées :
  - *Ne pas spawner la boucle des non-actives* : casse le switch instantané
    (l'instance et son snapshot doivent préexister), casse la migration de
    subscribers et le push d'une scène non-active (criterion #10 ADR 004).
  - *Stop/Start de la goroutine à chaque switch* : churn de goroutines, fenêtres
    de course avec la migration des subs et `CancelExec`, sémantique de restart
    ambiguë (Stop actuel = teardown définitif, show.go:216). Complexité sans
    gain : la quiescence par non-routage donne le même résultat observable.
- **Linearisation** : le point de bascule est le flip de `sh.active` sous lock.
  Un write concurrent au switch est routé vers la scène active *au moment où
  l'inbox lit le pointeur* — un event en vol pendant un switch peut atteindre la
  scène sortante ; acceptable et documenté (les events sont live-only par
  nature, driver 1).

### 3.2 Cycle de vie de l'état : freeze-and-resume

- **Désactivation** : l'état (`__vars`, compteurs, leaves) est **gelé tel quel**
  dans l'instance. `CancelExec` (existant, show.go:290) tue les tâches vives,
  les continuations parkées et les timers de l'instance ; une completion async
  arrivant après l'annulation est droppée (comportement actuel des resumes sur
  tâche annulée — réaffirmé, testé).
- **Réactivation** : la scène **reprend son état gelé** (pas de reseed) et
  `FireOnStart` refire (comportement actuel de SetActive, ADR 003 § 3.1.4
  réaffirmé). Le `scene_changed` + snapshot frais émis à la migration reflète
  cet état gelé — exactement ce que l'invariant prouvé live exerce déjà.
- Alternative écartée : *reseed defaults à chaque activation*. Elle casserait
  les scènes dont l'état doit survivre aux allers-retours (leaderboard,
  compteurs de show) et dupliquerait les chemins de reset existants. Le reset
  canonique reste : **re-push** (restart-reseed, show.go:154) ou **restart
  process** (criterion #11 ADR 004 — aucun état live persisté).
- Conséquence assumée : les leaves d'input (`__inputs.platform.*`, `__events.*`)
  d'une scène dormante **ne s'accumulent pas** — à la réactivation elles portent
  leur dernière valeur d'avant dormance, jusqu'au prochain event. C'est la
  doctrine (dormant = rien ne se passe), pas un bug.

### 3.3 on-event : déclaration d'acceptance au compile, miroir platform-stream

Le compilateur synthétise, par topic distinct des entrées on-event d'un graphe,
une déclaration d'acceptance `ExternalAdapter{Kind: "event-topic",
TargetPaths: ["__events.<event_name>"]}` — miroir exact de
`platformStreamBindings` (compile.go:888) : **pure acceptance**, aucune
goroutine (les starters poller/pg-listen filtrent déjà sur leur Kind), topics
triés pour le déterminisme du hash `scene_version`.

- Combiné au routage § 3.1 : un write `__events.<topic>` atteint l'entrée
  on-event de la **scène active seulement** — le gap est fixé *et* la doctrine
  tenue d'un même mouvement. Le firing lui-même existe déjà
  (`scene.go:656` → `execOnEvent`).
- `CanWritePath` inchangé : operator/admin écrivent partout ; un service token
  doit porter `__events.*` (ou un préfixe plus étroit) dans son allow-list
  `paths` pour fire un event — **aucun scope n'est élargi par défaut**.
- Alternative écartée : *accepter le préfixe `__events.` en dur dans
  `sceneAcceptsPath`*. Plus simple, mais brise la discipline « la scène déclare
  ce qu'elle accepte » (ADR 004 § 5), accepte des writes vers des topics que
  personne n'écoute (audités puis silencieusement perdus), et prive l'authoring
  d'une surface introspectable (les topics déclarés deviennent visibles dans le
  graphe compilé).

### 3.4 Invariant de switch : préservé, vérifié

`SetActive` ne change pas : migration des subs, `scene_changed` + snapshot,
`CancelExec`/`SetOnAir(false)` sur la sortante, `SetOnAir(true)` +
`FireOnStart` sur la destination (ordre FIFO inbox conservé). Le seul delta est
*en amont* (le routage inbox/tick suit le pointeur `active`). Séquence au
switch A→B : exec de A annulée et A dé-routée ; B routée, on-start fire, rendu
suit via la migration. Un test d'intégration A→B→A asserte : B exécute, A
n'exécute plus, l'état de A est intact au retour.

### 3.5 Quasar / `__inputs.platform.*`

Sous § 3.1, les writes de Quasar (service token scopé `quasar.twitch.*` /
`__inputs.platform.*`) n'atteignent que la scène active — les bindings
`platform-stream` des scènes dormantes sont inertes par non-routage. Les events
Twitch n'animent que l'antenne : cohérent avec la doctrine et avec le batch
`quasar.twitch.*` restant. La subscription writer détachée
(`SubscribeLiveWriter`, show.go:393) est orthogonale (elle gouverne ce que le
writer *reçoit*, pas ce qu'il écrit) — inchangée.

### 3.6 Hors périmètre

Test sessions et clones de validation : non gatés (ADR 006 § 3.4 réaffirmé —
`triggersGated` reste false hors roster), routés par leur propre canal, jamais
par l'inbox live. Aucun changement.

## 4. Consequences

- `active` gouverne désormais **rendu ET exécution** — le modèle mental devient
  « une seule scène vit ; le roster est un backstage gelé ».
- ADR 004 § 5 rule 4 est superseded sur le point fan-out (le reste de § 5 —
  acceptance déclarée — tient toujours et est renforcé).
- ADR 006 § 3.4 devient la couche 2 (défense en profondeur) d'un gating qui se
  fait désormais au routage.
- Métriques : `orion_inbox_dropped_total` reste valable (porte sur l'inbox de
  la scène routée) ; ajouter un compteur de writes absorbés (path non déclaré
  par l'active) serait utile mais non bloquant.
- Les blueprints multi-scènes « ambiants » (une scène backstage qui
  pré-calculerait en continu) deviennent impossibles par construction — choix
  doctrinal explicite du porteur.

## 5. Risks

- **R1 — write en vol pendant un switch** routé vers la scène sortante
  (fenêtre de quelques ms). Accepté : events live-only, perte équivalente à un
  event arrivé une frame plus tôt. Documenté § 3.1.
- **R2 — état stale à la réactivation** (leaves d'input figées) surprenant pour
  un auteur. Mitigation : on-start refire à chaque activation — un blueprint qui
  veut du frais le re-fetch dans sa chaîne on-start (pattern déjà canonique,
  live-testing.md).
- **R3 — régression de l'invariant de switch** si le routage est implémenté
  dans `SetActive` plutôt qu'à l'inbox. Mitigation : le découpage impose le
  changement côté inbox/tick uniquement, SetActive intouché ; test A→B→A requis
  (§ 6.4).
- **Sécurité — surface non élargie** : `CanWritePath` inchangé ; le routage
  *rétrécit* le rayon d'un write (1 scène au lieu de N) ; l'acceptance
  `event-topic` est dérivée du blueprint authored, même modèle de confiance que
  `platform-stream`. **Pas de clearance Bastion requise** pour cet ADR. Point
  de vigilance futur : le jour où un service token est minté avec un scope
  `__events.*` (ex. Cosmos qui fire des events), ce minting-là passe par
  Bastion.

## 6. Resolution criteria (testables)

1. **Routage actif-seul** : un write accepté (operator ou service) n'est
   délivré qu'à la scène active ; une scène chargée non-active ne reçoit ni
   write, ni tick (assert sur son state inchangé + zéro fire, modèle
   `exec_onair_test.go`).
2. **on-event live** : push d'une scène avec une entrée on-event
   (`event_name: "goal"`) → `sceneAcceptsPath` accepte `__events.goal` ; un
   write operator `__events.goal` fire l'entrée de la scène active ; le même
   write ne fire **pas** une scène chargée non-active écoutant le même topic.
3. **Freeze-and-resume** : A active accumule `__vars.x` ; switch A→B ; writes
   vers des paths que A déclarait n'altèrent pas l'état de A ; switch B→A :
   snapshot reflète l'état gelé de A, on-start refire, A ré-exécute.
4. **Invariant de switch intact** : la suite existante
   (`TestShow_SwitchMigratesLiveSubsAndEmitsSceneChanged`, criterion #5
   ADR 004 ≤ 100 ms) reste verte sans modification de ses assertions.
5. **Quasar actif-seul** : un write `__inputs.platform.twitch.*.last_chat`
   n'atteint que la scène active même si une scène dormante déclare le même
   binding platform-stream.
6. **Hash déterministe** : deux compiles du même blueprint avec on-event
   produisent le même `scene_version` (topics triés).
7. **Cancellation à la désactivation** : tâches vives, continuations parkées et
   timers de la scène sortante morts après switch ; une completion async
   arrivant post-switch est droppée sans effet.

## Amendment 1 — 2026-06-13 — sémantique d'activation (status: accepted)

> Author: Atlas · Deciders: @ClodoCapeo · Validation: Vigil (proposed→accepted, 2026-06-13) ·
> Décision du porteur sollicitée (gated). N'altère aucune décision §3.1–§3.6 :
> précise le contrat de §3.2 (réactivation) et §3.4 (invariant de switch) là où
> la rédaction disait « FireOnStart refire à chaque activation » alors que le
> code ne le tient que sur transition `from != id`.

### A1.1 Le défaut corrigé

§3.2 et §3.4 affirment trois fois (« on-start refire à chaque activation »,
R2 mitigation §5) que la réactivation d'une scène refire `on-start`. Le code ne
l'honore que sur une **transition** : `SetActive` (`show.go:499`) gate
`SetOnAir(true)` + `FireOnStart` derrière `if from != id`. Conséquence : un
`POST /show/active-scene` sur la scène **déjà active** (`from == id`) ne refire
rien — la logique `on-start` (ici un `core.http.request@1`) ne tourne jamais.

C'est un **écart implémentation ↔ doctrine déjà écrite**, pas un nouveau choix.
Au boot le bug est masqué par accident : `loadActiveScenes` (`main.go:355`)
appelle `SetActive` avec `sh.active == ""`, donc `from="" != id` fire on-start.
Une réactivation post-boot, idempotente ou après reload, ne refire pas.

### A1.2 Décision — Option A (l'activation est l'unité de (re)lancement d'exec)

**`POST /show/active-scene` (re)fire `on-start` à chaque appel, y compris
`from == id`.** L'activation est le verbe canonique unique de mise/maintien à
l'antenne ET de (re)lancement de l'exec d'une scène ; il n'y a **pas** de verbe
`restart`/`reload` séparé (Option B et C écartées, A1.5).

Sémantique précise du cas `from == id` (ré-activation de l'active) :

1. **Exec** : `dest.CancelExec()` (tue tâches vives, continuations parkées,
   timers — exactement la cancellation §3.2/§5 R-cancel), puis `SetOnAir(true)`
   (idempotent), puis `dest.FireOnStart("system:scene-reactivated")`. Un seul
   fire par appel (pas de double-fire : c'est `SetActive` qui fire, jamais le
   chemin de migration des subs).
2. **État (`__vars`, compteurs, leaves)** : **préservé, gelé — pas de reseed.**
   On reste sous la doctrine freeze-and-resume §3.2 : l'activation ne reset pas
   l'état. Le reset reste le re-push (`show.go:154`) ou le restart process
   (criterion #11 ADR 004). Une scène leaderboard ré-activée garde ses scores ;
   son `on-start` se ré-exécute par-dessus l'état gelé (idempotent par
   construction côté authoring — un `on-start` qui fetch/écrit recalcule, un
   `on-start` qui incrémente est un bug d'authoring, hors périmètre moteur).
3. **Rendu / subs** : pas de `scene_changed` quand `from == id` (pas de
   transition viewer — règle phantom-transition `show.go:543` réaffirmée). Le
   snapshot frais réémis par le re-fire couvre la resync des valeurs.

### A1.3 Impact sur `show.go::SetActive`

Le gate exec doit sortir du `if from != id`. Forme attendue (Forge tranche
l'implémentation exacte) :

- **`from != id`** (switch A→B) : inchangé — `prev.CancelExec()` +
  `prev.SetOnAir(false)` sur la sortante ; `dest.SetOnAir(true)` +
  `dest.FireOnStart` sur la destination ; migration + `scene_changed` + snapshot.
- **`from == id`** (ré-activation) : `dest.CancelExec()` puis `SetOnAir(true)`
  (idempotent) puis `dest.FireOnStart`. **Pas** de `CancelExec`/`SetOnAir(false)`
  sur « prev » (prev == dest : ne jamais s'éteindre soi-même), **pas** de
  `scene_changed`. Le snapshot frais est réémis aux subs existants.

Ordre FIFO inbox (SetOnAir avant FireOnStart) conservé dans les deux branches —
invariant ADR 008 §3.4.

### A1.4 Impact sur le chemin de boot (`loadActiveScenes`)

Aucun changement de code requis : `from="" != id` reste une transition, on-start
fire comme aujourd'hui. **Mais la dépendance accidentelle est levée** — le boot
ne fire plus parce que `from` est vide *par chance*, il fire parce que A1.2
garantit le fire sur toute activation. Le comportement de boot est désormais le
cas particulier d'une règle générale, pas une coïncidence. **Le contrat de boot
est explicité : la scène active persistée (re)exécute son `on-start` au
démarrage d'Orion** (réponse à la question ouverte de l'Option C — la scène
persistée n'est pas gelée jusqu'à une activation manuelle ; le reseed du
pointeur EST une activation).

### A1.5 Alternatives écartées

- **Option B (statu quo + verbe `restart` séparé).** Écartée : multiplie la
  surface opérateur pour un cas que l'opérateur vit comme « je ré-active »
  (idempotence attendue), contredit la rédaction déjà votée de §3.2/§3.4, et
  laisse le piège vivant (« scène inactive après restart » — `live-testing.md`).
  Un second verbe n'achète aucune sémantique que A1.2 ne couvre déjà.
- **Option C (séparer « activer rendu » de « (re)lancer exec »).** Écartée :
  introduit deux cycles de vie distincts (rendu vs exec) là où ADR 008 §4 a
  précisément unifié « `active` gouverne rendu ET exécution ». Re-fracturer le
  modèle mental quelques jours après l'avoir unifié n'est pas justifié. La part
  utile de C (clarté du boot) est absorbée par A1.4.
- **Reseed de l'état à la réactivation.** Déjà écartée en §3.2 — réaffirmée :
  A1.2 fire `on-start` SANS reset d'état. Refire ≠ reseed.

### A1.6 Resolution criteria (testables — étendent §6)

8. **Ré-activation idempotente refire on-start** : scène A active ;
   `POST /show/active-scene{A}` une seconde fois (`from == id`) → `on-start` de A
   refire exactement une fois (assert : un `core.http.request@1` / un compteur de
   fire en on-start s'incrémente d'exactement 1 par appel ; pas de double-fire).
9. **Ré-activation préserve l'état** : A accumule `__vars.x = N` ;
   `POST /show/active-scene{A}` → après le re-fire, `__vars.x` n'est PAS reseedé
   aux defaults (vaut N, ou la valeur recalculée par l'on-start par-dessus N —
   jamais le default sec). Distingue refire (oui) de reseed (non).
10. **Pas de phantom `scene_changed` en ré-activation** : `from == id` n'émet
    aucun `scene_changed` aux subs ; un snapshot frais est émis. La scène ne
    s'éteint jamais elle-même (aucun `SetOnAir(false)` observé sur A).
11. **Invariant de switch intact** : `TestShow_SwitchMigratesLiveSubsAndEmits`
    `SceneChanged` (§6.4 / criterion #5 ADR 004) reste vert sans modification de
    ses assertions — la branche `from != id` n'a pas bougé.
12. **Boot (re)exécute l'active persistée** : Orion démarre avec
    `active_scene_id` persistée → la scène revient à l'antenne ET son `on-start`
    s'exécute (assert sur le fire au cold start ; le piège `live-testing.md`
    « scène inactive après restart » est couvert par un test, plus par une
    vérification manuelle post-deploy).

### A1.7 Sécurité

Aucune surface élargie : `CanWritePath` et l'auth de `POST /show/active-scene`
(operator/admin via ZabGate) inchangés. Un re-fire idempotent ne crée aucun
nouveau chemin d'écriture ; il ré-exécute un `on-start` authored déjà sous le
modèle de confiance ADR 008/006. **Pas de clearance Bastion requise** (cohérent
avec §5 — vigilance Cosmos/`__events.*` inchangée). Vigil valide
proposed→accepted ; Forge implémente sur `forge/<issue>-reactivation-fires-onstart`.
