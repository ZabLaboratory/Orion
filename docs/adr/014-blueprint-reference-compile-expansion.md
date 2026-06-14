# ADR 014 — Blueprint-reference support via compile-time subgraph expansion

- **Status**: proposed
- **Date**: 2026-06-14
- **Decided**: —
- **Deciders**: @ClodoCapeo
- **Author**: Atlas
- **Supersedes**: — (étend ADR 003 §3.4 partition par-blueprint + ADR 004 §7 contrat manifest ; ne renverse rien)
- **Superseded by**: —

## 1. Context

Le moteur **Orion** (runtime réactif Go, chemin live LSDP : event → Orion →
LSDP → Solar) **ne sait pas exécuter un nœud `reference: {blueprint_id, version}`**
— l'appel d'une blueprint-fonction Blue publiée comme sous-graphe. Ce mécanisme
n'existe qu'en **Blue** (`Blue/src/blue/services/executor.py` : `_run_subgraph`,
`_blueprint_ref_key`, chemins `/blueprints/{id}/execute` et `resolve`), comme
interpréteur récursif sur graphe fetché en DB, memoïsé par `(blueprint_id, version)`.

Gap moteur, vérifié sur `main` :

- `ComputeManifestEntry` (`internal/compiler/types.go:286`) ne porte **que des
  métadonnées** : `is_pure`, `is_bounded`, `declared_inputs`, `declared_output_type`,
  `version`. **Aucun graphe interne.** Le wire DTO Blue `blueManifestEntry`
  (`types.go:314`) non plus. → pas d'expansion possible au compile aujourd'hui.
- `compile.go:665` valide chaque nœud par `manifest[n.Compute]` — lookup par id
  de compute (`namespace.name@version`). Un nœud `reference` n'a pas de compute
  servi par le registry → `ErrUnknownComputeNode`.
- `internal/runtime/compute.go` : `ComputeRegistry` = map plate d'impls Go
  `core.*@1` ; `Get(id)` échoue hors registry. Aucun mécanisme de sous-graphe.
- `internal/conformance/conformance.go` : gate CI — tout id du manifest doit être
  servi par le registry runtime OU allowlisté.
- Le topo-sort est **par blueprint** (`compile.go:170`, ADR 003 §3.4 : aucune
  arête cross-blueprint) ; `detectComponentCycles` (`compile.go:453`) +
  `CYCLIC_COMPONENT` existent déjà.
- Preuve : la scène de réf `bp-composite-chat-draft-scene` est **100 % plate**
  (inline `core.*`, zéro `reference`).

**Décision porteur déjà actée** : on NE veut PAS inliner/dupliquer la logique à
la main (drift). Orion doit **supporter les blueprint-references** pour que les
scènes live réutilisent les blueprint-fonctions publiées comme **source unique** —
cohérent avec [[orion-must-serve-all-of-blue]] (ADR Orion full-Blue : ne jamais
restreindre le langage servi ; la sûreté est un gate d'authoring, pas une
limitation moteur).

**Driver concret à débloquer.** Une scène overlay LSML (validée visuellement,
`ZabCanvas/scripts/build_canvas_chat_sponso_mockup.py`, branche
`forge/canvas-chat-sponso-mockup`) doit être pilotée live par commande chat
(`lck`/`lec` → `match_id`) en réutilisant 3 blueprint-fonctions Blue **publiées** :
- `match-roster` (`50bacf4d…` v2) : `match_id` → `rows[10] {summoner_name,
  champion, side, role, player_id}`
- `score-for-player` (`74d12dfc…` v1) : `match_id + player_id` → `score`
- `score-to-color` (`525369d9…` v3) : `score` → `{color, text}`
Repaint live via deltas LSDP au changement de `match_id`.

## 2. Decision drivers

- **Source unique / anti-drift** (doctrine porteur) : la logique vit une seule
  fois, dans la blueprint-fonction publiée. Orion la consomme, ne la recopie pas.
- **Budget delta live ≤ 50 ms** (ADR 004 crit #4) : le chemin réactif chaud ne
  doit pas payer une résolution/exécution récursive par tick.
- **Conformance gate intacte** (`conformance.go`) : tout id servi runtime a un
  executor + test. On ne veut pas créer une classe de nœuds qui échappe au gate.
- **Détection de cycle** : un `reference` introduit un graphe d'appel ; il faut
  un cycle-check **inter-blueprint** (le `CYCLIC_COMPONENT` actuel est
  intra-graphe).
- **Versioning / pinning** : `reference.version` est explicite (immutabilité des
  versions publiées Blue) — la résolution doit pinner exactement cette version,
  jamais `current_version`.
- **Purity / bounded propagés** : Blue dérive déjà la purity **récursivement**
  des nœuds référencés (CLAUDE.md Blue, §2026-05-02). Orion doit hériter de cette
  métadonnée, pas la recalculer.
- **Grain leaf LSDP** : seuls scalaire / array-de-scalaires passent le wire
  (`live-testing.md` §LSDP). Une fonction qui retournerait un objet imbriqué doit
  être lowerée en leaves scalaires, comme aujourd'hui.
- **Active-scene-only (ADR 008)** : l'expansion ne change rien au scope — c'est
  un artefact de compile, pas de scheduling.
- **Déterminisme / idempotence** : `scene_version` (hash) doit rester
  déterministe ; deux pushes du même graphe + mêmes versions référencées → même
  bundle.
- **Coût moteur le plus bas** (on est sur Fable).

## 3. Decision

**Option (2a) — Expansion au compile-time. Décision retenue.**

Le compilateur Orion **expanse récursivement** chaque nœud `reference:
{blueprint_id, version}` en son sous-graphe `core.*` plat, **avant** la
validation manifest et la conformance. Le runtime **ne change pas** : il ne voit
jamais un `reference`, seulement des `core.*` déjà connus de son registry.

Mécanique :

1. **Contrat manifest étendu (Blue ↔ Orion) — ressort Conduit.** Le manifest
   reste métadonnées ; on ajoute un **endpoint de résolution de graphe** côté
   Blue (le compilateur Orion fetch le graphe d'une `(blueprint_id, version)`
   référencée — Blue a déjà `/blueprints/{id}/resolve` et le DB lookup memoïsé de
   `_run_subgraph`). Orion fetch **au push uniquement** (pas au runtime), memoïse
   par `(blueprint_id, version)` le temps du compile, et inline. Le contrat
   précis (endpoint vs enrichir le manifest avec les graphes ; pinning par
   version ; forme du graphe servie) est **à arrêter par Conduit** (cf. §4 et
   issue dédiée). Recommandation Atlas : **endpoint de résolution dédié** plutôt
   qu'embarquer tous les graphes dans `/_compute-manifest` (le manifest reste
   léger ; on ne fetch que les fonctions réellement référencées par la scène
   poussée).

2. **Expansion récursive (compilateur Orion).** Nouvelle passe **avant**
   `manifest[n.Compute]` (`compile.go:665`) : pour chaque nœud `reference`,
   résoudre `(blueprint_id, version)`, fetch le sous-graphe, **mapper ses inputs/
   outputs** sur les pins du nœud appelant (alpha-renommage des node-ids pour
   éviter les collisions, comme `_run_subgraph` qui donne une activation record
   fraîche), inliner les nœuds `core.*` dans le graphe parent, recommencer sur
   les `reference` imbriqués. Résultat : un graphe **100 % plat** identique en
   nature à `bp-composite-chat-draft-scene`. La validation manifest et le
   topo-sort par-blueprint existants s'appliquent **inchangés** au graphe expansé.

3. **Cycle-check inter-blueprint.** Pendant l'expansion, maintenir une pile
   d'appel `(blueprint_id, version)`. Un retour sur une paire déjà dans la pile →
   nouveau diagnostic **`CYCLIC_BLUEPRINT_REFERENCE`** au push (réutilise la
   sémantique de `CYCLIC_COMPONENT` mais sur le graphe d'appel, pas le graphe de
   nœuds). Borne de profondeur de récursion (réutilise l'esprit du `step_budget`
   partagé de `_run_subgraph`).

4. **Pinning par version.** La résolution pinne **exactement** `reference.version`
   (versions publiées Blue immutables). Référence à une version inexistante /
   non publiée → diagnostic `BLUEPRINT_REF_UNRESOLVED` au push. Aucune résolution
   `current_version` implicite.

5. **Purity / bounded.** Le nœud expansé hérite des métadonnées du sous-graphe
   telles que servies par Blue (déjà dérivées récursivement côté Blue). Orion ne
   recalcule pas — il consomme. Cohérent avec ADR 006 (purity = scheduling
   metadata, plus un reject gate).

6. **Conformance & déterminisme.** Comme le runtime ne voit que des `core.*`,
   `conformance-matrix` reste vert sans changement de surface runtime (aucun
   `EXEC_OP_UNMAPPED`/id non servi). Le `scene_version` hash est calculé sur le
   graphe **expansé** (déterministe : même version référencée → même expansion →
   même hash). Un changement de version d'une fonction référencée change le hash
   — correct (c'est une scène différente).

7. **Grain leaf LSDP.** L'expansion ne crée pas d'objet sur le wire : les outputs
   de la fonction sont lowerés en leaves scalaires / array-de-scalaires par le
   chemin de lowering existant, comme tout `core.*` plat. Une fonction dont
   l'output ne se lowere pas en scalaire est rejetée par les règles de binding
   actuelles — pas une nouveauté.

**Pourquoi pas (2b) — exécuteur runtime.** Un sous-graphe récursif en Go au
runtime, à parité avec `_run_subgraph`, est rejeté :
- **Perf** : il fait payer la résolution/exécution récursive sur le chemin chaud
  (≤ 50 ms par delta, ADR 004 #4). L'expansion au push paie le coût **une fois**,
  jamais par tick.
- **Surface runtime** : il introduit une nouvelle classe d'exécution (scheduling
  réactif : quelles leaves arment quel sous-graphe, freeze-resume ADR 008,
  re-entrancy) — beaucoup plus de surface à tester et à durcir, sur le composant
  le plus critique (le runtime live).
- **Conformance** : il crée des nœuds servis runtime qui ne sont pas dans le
  registry plat → il faut élargir le gate de conformance, exactement ce qu'on
  veut éviter.
- **Coût** : (2a) ne touche **pas** le runtime. C'est le chemin le moins coûteux
  pour la doctrine full-Blue, donc le bon choix sur Fable.

**Hybride écarté.** Pas de raison de garder un chemin runtime : aucune fonction
de la scène driver n'est récursive-dynamique (toutes sont des transforms pures
data-bound). Si un jour une blueprint-fonction devait être **dynamiquement
sélectionnée au runtime** (id de fonction calculé en live), ce serait une autre
ADR avec son propre threat-model — pas un besoin actuel.

## 4. Consequences

**Contrat Blue ↔ Orion (Conduit).** Le manifest seul ne suffit plus pour les
scènes à references : Orion doit pouvoir **fetch le graphe d'une fonction
référencée à une version pinnée**, au push. Conduit arrête : endpoint dédié
(recommandé) vs enrichissement manifest ; forme du graphe servie (réutiliser
`/blueprints/{id}/resolve` / le DB lookup de `_run_subgraph`) ; comportement
version inexistante. Versioning : strictement par `reference.version`.

**Orion compilateur.** Une passe d'expansion récursive avant la validation
manifest ; un client HTTP de résolution (au push, memoïsé) ; un cycle-check
inter-blueprint (`CYCLIC_BLUEPRINT_REFERENCE`) ; le `scene_version` hash calculé
sur le graphe expansé. Le topo-sort par-blueprint et la validation manifest
**ne changent pas** (ils opèrent sur le graphe déjà plat).

**Orion runtime.** **Zéro changement.** Le registry plat, le scheduling réactif,
l'active-scene-only (ADR 008), le grain leaf LSDP restent intacts.

**Blue.** Selon le contrat Conduit : exposer (ou confirmer) l'endpoint de
résolution de graphe par `(blueprint_id, version)`. La logique de résolution
existe déjà (`_run_subgraph`, `_blueprint_ref_key`, cache memoïsé) — c'est
surtout la surface HTTP à figer.

**Conformance.** Inchangée en nature ; un nœud `reference` n'apparaît jamais
runtime. Test à ajouter : une scène avec `reference` expanse en `core.*` tous
servis (pas d'`EXEC_OP_UNMAPPED`).

**Déterminisme.** Push idempotent : même graphe + mêmes versions référencées →
même bundle byte-stable.

**Driver scène data-bound.** Une fois (2a) en place : on peut authorer une
blueprint compositrice **plate côté commande** (binding LSML + commande chat
`lck/lec → match_id`) qui appelle les 3 fonctions par `reference` ; Orion les
expanse au push ; le repaint live se fait par deltas LSDP au changement de
`match_id`. C'est l'issue terminale.

## 5. Risks

- **Explosion de graphe** : un sous-graphe profond/large gonfle le graphe
  expansé. Mitigation : borne de profondeur + métrique de taille de graphe
  post-expansion ; refus `BLUEPRINT_REF_EXPANSION_LIMIT` au-delà d'un seuil. Pas
  un risque runtime (le coût est au push).
- **Cycle de références** : couvert par `CYCLIC_BLUEPRINT_REFERENCE` (pile
  d'appel). Doit être testé explicitement (A→B→A et auto-référence).
- **Dérive de version** : si Blue servait `current_version` au lieu de la version
  pinnée, la scène changerait silencieusement de comportement. Mitigation :
  pinning strict + test que la résolution refuse une version absente.
- **Skew manifest/graphe** : le graphe résolu doit être cohérent avec les
  métadonnées du manifest (purity/bounded). Mitigation : Orion consomme la purity
  servie par Blue, ne la recalcule pas — pas de double source.
- **Sécurité** : la résolution est un **fetch HTTP interne** Orion → Blue via
  ZabGate (même trust model que le manifest actuel, ADR 004 §7 ; aucun secret,
  aucune sortie réseau externe, aucune surface d'attaque nouvelle). **Bastion non
  requis.** Signalé pour traçabilité ; si la résolution exposait un nouveau scope
  cross-service ou un endpoint public, ce serait une autre ADR avec clearance
  Bastion.

## 6. Resolution criteria (testables)

1. **Contrat (Conduit)** : un endpoint/extension permet à Orion de fetch le
   graphe d'une `(blueprint_id, version)` publiée ; version absente/non publiée →
   erreur typée ; pinning strict par `version` (jamais `current_version`). Test
   contrat round-trip Blue↔Orion.
2. **Expansion (compilo Orion)** : une scène avec un nœud `reference` vers une
   fonction publiée compile en un graphe **100 % `core.*` plat** ; les inputs/
   outputs du nœud appelant sont mappés sur les pins de la fonction ; les node-ids
   sont alpha-renommés sans collision. (test compilateur)
3. **Récursion** : une fonction qui en référence une autre (réf imbriquée)
   expanse correctement sur N niveaux ; borne de profondeur respectée. (test)
4. **Cycle** : A→B→A et auto-référence → `CYCLIC_BLUEPRINT_REFERENCE` au push,
   pas de boucle infinie. (test compilateur)
5. **Pinning** : deux versions différentes de la même fonction référencées
   produisent deux expansions différentes ; version inexistante →
   `BLUEPRINT_REF_UNRESOLVED`. (test)
6. **Conformance** : `TestConformance_Matrix` reste vert ; une scène à `reference`
   expansée ne contient aucun id non servi runtime (pas d'`EXEC_OP_UNMAPPED`).
7. **Déterminisme** : deux pushes du même graphe + mêmes versions référencées →
   `scene_version` hash identique, bundle byte-stable. Changer la version d'une
   fonction référencée change le hash. (test)
8. **Perf** : aucun coût de résolution/exécution récursive au runtime ; le delta
   reste ≤ 50 ms (ADR 004 #4) sur une scène expansée. La résolution n'a lieu qu'au
   push. (test/mesure)
9. **Driver live (critère porteur)** : la scène overlay LSML pilotée par commande
   chat (`lck/lec → match_id`), réutilisant `match-roster` + `score-for-player` +
   `score-to-color` par `reference`, repeint à l'antenne au changement de
   `match_id`, prouvé par record `.mp4` (`live-testing.md`, pas un snapshot wire).
   C'est le seul critère de « fini » du chantier.

CI verte + deploy vert (`GET /orion/api/v1/health` via ZabGate) restent le gate
de merge standard (`CLAUDE.md` Orion). Rollback : voir §7.

## 7. Rollback

L'expansion est une passe de compile **additive** : aucun changement runtime,
aucune migration. Rollback = revert du PR compilateur (et, si déjà mergé, du PR
Blue/contrat). Les scènes existantes 100 % plates (sans `reference`) sont
**inchangées** — la passe d'expansion est un no-op sur un graphe sans `reference`,
donc le revert n'affecte aucune scène live actuelle. Aucun état persisté à
défaire (le bundle est content-hashed et recompilable).

## 8. Liens

- ADR 003 §3.4 — partition exec + topo-sort **par blueprint**, pas d'arête
  cross-blueprint (base sur laquelle l'expansion produit des graphes plats).
- ADR 004 §7 — contrat manifest Blue↔Orion (`/_compute-manifest`,
  `/_validate-bindings`) ; crit #4 budget delta ≤ 50 ms.
- ADR 006 — purity = scheduling metadata (plus un reject gate) ; hérité, pas
  recalculé.
- ADR 008 — active-scene-only execution : inchangé (l'expansion est compile-time).
- `internal/conformance/conformance.go` — gate registry/manifest, reste vert.
- Blue `_run_subgraph` / `_blueprint_ref_key` (`services/executor.py`) — modèle
  de résolution récursive memoïsée par `(blueprint_id, version)` à porter au
  contrat de résolution.
- Doctrine : [[orion-must-serve-all-of-blue]], [[transitions-are-authored-scenes-not-code]].
