# ADR 016 — Profil d'exécution `embedded-local` : Orion embarqué dans Prism

- **Status:** accepted
- **Date:** 2026-06-21
- **Decided:** 2026-06-21
- **Deciders:** @ClodoCapeo
- **Author:** Atlas
- **Supersedes:** —
- **Superseded by:** —

> **Portée croisée Orion ↔ Prism.** ADR Orion (runtime/profil) au repo Orion, avec un
> volet **packaging Prism** (sidecar bundlé, lifecycle Electron). Pas d'ADR Prism
> miroir : le packaging suit le précédent Pulsar (`@clodocapeo/pulsar-bundle-full`,
> `broadcast-engine.ts`) déjà en place ; cet ADR le décrit comme conséquence, pas
> comme nouvelle doctrine Prism.

## 1. Context

**Décision porteur ferme.** Le moteur d'exécution local **ne doit dépendre d'aucun
Docker externe ni de la stack microservices**. Il doit être **embarqué dans Prism**
(Electron desktop), comme Prism embarque déjà Pulsar (fork OBS, sidecar binaire géré
par le main process) et vendore Solar. Le client ouvre Prism → le mode local exécute
des scènes Blue localement, **zéro infra externe**. Si le client doit lancer Docker,
le produit perd son intérêt.

**Invariant cardinal : `local == antenne`.** Même **binaire moteur** Orion, même
**contrat opérateur** (ADR 008 : `/cockpit/contracts` + `/operator/*`), même cockpit.
**Seule la plomberie** (store, auth, data, fetch upstream) change **par profil**
(config/flags + sidecars bundlés), **jamais un fork du code moteur**. Tout backend
divergent (store/auth) qui crée un 2ᵉ chemin de code non exercé à l'antenne est une
régression de cet invariant.

**Cible MVP** : exécuter la scène **canvas-chat-sponso** (LCK/LEC + scores) en local
embarqué, `db.query` inclus. Note produit liée : le déclencheur « commande chat
lck/lec » est remplacé par **deux `core.operator.on-call@1` LCK/LEC** → deux boutons
cockpit (phase A), puis un `core.operator.await-value@1` + recherche DB pour choisir
n'importe quel match (phase B). L'embarquement doit supporter ces deux flux.

**Grounding (vérifié par grep ciblé) :**

- La boucle moteur (tick + inbox + scènes réactives + entrypoints + RenderBundle→Solar
  WS) tourne **sans dépendance entrante** Twitch/Quasar (ce sont des sorties). Config
  **entièrement env-driven** (`internal/config/config.go`) ; **`LSDPMode` est un
  précédent de profil-par-flag** (`bespoke`/`dual`/`lsdp`).
- **Trois des quatre abstractions de découplage existent déjà** :
  - Le **compilateur prend un `Fetcher`** (`FetchCanvasLayout`/`FetchBlueprint`,
    `internal/compiler/compile.go`) — interface, pas un appel HTTP en dur.
  - Le `db.query` passe par un **`DBQueryClient` construit avec une URL + un
    `tokenFn`** (`internal/effects/dbquery.go`) — Orion ne tient aucun credential ni
    pool DB, il POST un descripteur à `${ZABGATE}/<svc>/api/v1/_query`.
  - L'auth est `auth.Identity` via **`FromHeaders`** (`internal/auth/identity.go`,
    `X-Authenticated-User/Role/Paths`) ; `requireOperator` (`internal/api/public.go`)
    en dépend.
- **La seule dépendance dure sans abstraction** : le **store** est un `*Store`
  concret backé `pgxpool` (`internal/store/store.go`, migrations goose, jsonb). C'est
  l'unique nouvelle interface structurante à introduire.
- **Précédent d'embarquement Prism mûr** : `broadcast-engine.ts` lance Pulsar en
  **sidecar lazy keep-alive**, communication **loopback** uniquement (clés en clair
  jamais sur le wire), lifecycle géré par le main process ; `scene-server.ts` =
  Fastify loopback. Orion = binaire Go ⇒ **sidecar Orion managé par le main Prism**
  est directement précédenté ; l'in-process Go-dans-Node est exclu.

## 2. Decision drivers

- **D1 — Zéro infra client.** Aucune installation Docker, aucun service à démarrer
  côté utilisateur. Prism s'ouvre, le moteur tourne.
- **D2 — `local == antenne` (non négociable).** Même binaire, même contrats, mêmes
  chemins de code chauds ; le profil ne change que la **plomberie de bord**. On
  préfère un sidecar de données **identique en forme** à l'antenne plutôt qu'un
  backend store alternatif qui dédouble le chemin chaud.
- **D3 — Additif, pas de fork.** Les abstractions introduites sont des **interfaces**
  derrière lesquelles l'impl actuelle (pg/HTTP/headers) reste le défaut antenne ;
  l'impl locale est une seconde implémentation, sélectionnée par flag.
- **D4 — Sécurité de bord.** L'Orion embarqué **n'écoute que loopback**, jamais
  d'exposition réseau ; le shim d'auth local ne doit pas devenir un trou (cf. §5/§7).
- **D5 — MVP d'abord.** Le minimum viable = la scène canvas-chat-sponso jouable en
  local avec ses `db.query`. Tout ce qui n'est pas sur ce chemin est différé.

## 3. Decision

**GO.** On introduit un **profil d'exécution `embedded-local`** sélectionné par config,
qui fait tourner le **vrai binaire Orion** dans Prism via un **sidecar Go bundlé**,
avec **quatre dépendances collapsées** chacune **derrière une abstraction additive**.
Le profil antenne reste le défaut ; aucun chemin moteur n'est forké.

### 3.1 — Topologie d'embarquement (Question A)

Orion = **sidecar Go bundlé**, lancé/managé par le **main process Prism**, sur le
modèle Pulsar :

- **Bundling** : binaire Orion par OS (linux/mac/win) en `extraResources`
  electron-builder, ou wrapper npm `@clodocapeo/orion-bundle-<os>` aligné sur
  `pulsar-bundle-full`. Cross-compile Go (`GOOS`/`GOARCH`) en CI release.
- **Lifecycle** : **lazy keep-alive** (spawn au premier besoin local, arrêt propre au
  quit Prism), supervisé par le main (restart sur crash, health `GET /health`).
- **Ports** : `ORION_LISTEN_ADDR=127.0.0.1:<port>` + `ORION_INTERNAL_ADDR` loopback
  uniquement (D4). Prism découvre le port (fixe réservé ou handshake stdout).
- **Confirmé/raffiné** : oui, sidecar — c'est le précédent exact. Raffinement :
  réutiliser le pattern de découverte/health de `broadcast-engine.ts`, et **un seul
  sidecar par instance Prism** (single-live invariant déjà porté côté Pulsar).

### 3.2 — Collapse des 4 dépendances (Question B)

Principe transverse (D2) : pour chaque dépendance, on privilégie la stratégie qui
**garde le chemin de code chaud identique** à l'antenne et confine la divergence au
**branchement de bord** (driver/impl), pas au cœur.

**(1) Postgres → SQLite via une interface `Store`.**
- **Retenu** : extraire une **interface `Store`** (les méthodes déjà présentes :
  scenes, definitions, pushed versions, show_state, stream_rules, validations,
  assets) ; impl par défaut `pgStore` (actuelle, antenne) ; impl `sqliteStore`
  embarquée (fichier local sous le userData Prism). Migrations dupliquées en dialecte
  SQLite (le schéma est simple : tables + jsonb→`TEXT`/`json1`).
- **Écartée** : *embedded-postgres bundlé* (zonky/embedded-postgres-go). Coût : ~100+ Mo
  par OS, démarrage lent, complexité de lifecycle d'un vrai postgres enfant — contraire
  à D1/D5. SQLite est zéro-process, in-file.
- **Risque D2** : jsonb. Mitigation : le code n'utilise le jsonb que comme blob opaque
  (`json.RawMessage`) — pas de requête `->>`/GIN sur le chemin chaud ; un store SQLite
  qui stocke le même blob en TEXT préserve le chemin. **À auditer** : confirmer aucune
  requête jsonb-spécifique dans le store (sinon SQLite `json1` couvre `json_extract`).
- **Coût** : moyen (interface + 2ᵉ impl + migrations SQLite + tests de parité). C'est
  le plus gros poste.

**(2) Auth ZabGate/ZabAuth → `AuthSource` local (trust loopback).**
- **Retenu** : extraire une **`AuthSource`** derrière `FromHeaders`. Impl antenne =
  `headerAuth` (actuelle, lit `X-Authenticated-*` injectés par ZabGate). Impl locale =
  `localOperatorAuth` : **Prism injecte l'identité operator du user local** sur chaque
  requête loopback (header identique `X-Authenticated-Role: operator`, user =
  identité locale). **La sémantique de rôle est préservée** : `requireOperator`
  fonctionne à l'identique — c'est le **même chemin de code** (D2), seul change qui
  pose les headers.
- **Écartée** : *mode trust « tout passe » sans rôle*. Rejeté : casse D2 (bypass de
  `requireOperator`) et crée un trou si le port fuit (D4).
- **Sécurité** : valable **uniquement** parce qu'Orion n'écoute que loopback (D4) +
  un **secret de handshake** Prism→Orion (en-tête partagé au spawn) pour qu'un autre
  process local ne puisse pas se faire passer pour Prism. **→ Flag Bastion** (§5/§7).
- **Coût** : faible (le point d'injection est déjà centralisé ; Prism pose 2 headers).

**(3) Data sources ZabTruth/ZabRanking → sidecar data local *iso-contrat*.**
- **Retenu (D2-pur)** : un **sidecar data bundlé** qui expose **le même contrat
  `_query`** que ZabGate (`POST /<svc>/api/v1/_query`, descripteur → rows). Orion
  garde son `DBQueryClient` **inchangé**, pointé sur `ORION_ZABGATE_URL=127.0.0.1:<p>`
  (le sidecar local). Le sidecar lit des **miroirs SQLite** (truth/ranking) seedés à
  l'install/à la mise à jour. ⇒ le chemin `db.query` chaud est **bit-identique** à
  l'antenne.
- **Alternative (MVP-rapide)** : faire de l'Orion local un mode qui résout `_query`
  en interne contre des fixtures. Écartée pour le chemin chaud (dédouble la logique
  `_query` → viole D2) mais **acceptable en phase A** sous forme du sidecar minimal
  (fixtures LCK/LEC figées) tant que le contrat de surface reste `_query`.
- **Provenance des miroirs** : seed initial = export depuis ZabTruth/ZabRanking
  (snapshot) bundlé ; refresh ultérieur hors-scope MVP. **→ contrat à figer avec
  Conduit** (le `_query` local doit être strictement le contrat wire de ZabGate).
- **Coût** : moyen (sidecar + seed SQLite). Faible si phase A se contente de fixtures
  derrière le même endpoint.

**(4) Fetch compile-time `/canvas` + `/blue` → `Fetcher` local.**
- **Retenu** : 2ᵉ impl du **`Fetcher` déjà existant**. Impl antenne =
  `httpFetcher` (CanvasBaseURL/BlueBaseURL). Impl locale = `bundledFetcher` :
  **les artefacts matérialisés** (le layout ZabCanvas + les blueprints Blue de la
  scène) sont **bundlés/servis localement** (fichiers sous userData, ou servis par le
  sidecar data en (3)). Le compilateur ne voit aucune différence.
- **Écartée** : *embarquer Blue/ZabCanvas complets en local*. Rejeté : ramène la stack
  microservices que D1 interdit. On bundle l'**artefact compilé/figé** de la scène,
  pas les services d'authoring.
- **Coût** : faible (l'interface existe ; il faut un format de bundle de scène +
  l'impl de lecture).

### 3.3 — Profils & flags (Question C)

Un **`ORION_PROFILE`** additif sélectionne le câblage de bord ; **valeur par défaut
`antenne`** (comportement actuel strictement préservé) :

| Aspect | `antenne` (défaut) | `embedded-local` |
|---|---|---|
| Store | `pgStore` (`ORION_DATABASE_URL`) | `sqliteStore` (fichier userData) |
| Auth | `headerAuth` (ZabGate) | `localOperatorAuth` (handshake loopback) |
| DataSource `_query` | `ORION_ZABGATE_URL` distant | `ORION_ZABGATE_URL` = sidecar loopback |
| Fetcher | `httpFetcher` (Canvas/Blue) | `bundledFetcher` (artefacts locaux) |
| Listen | `0.0.0.0:4007` | `127.0.0.1:<p>` loopback only |

Abstractions à introduire (toutes **additives**, interface + impl actuelle = défaut) :
**`Store`**, **`AuthSource`**, **`Fetcher`** (existe déjà), DataSource resté à l'URL
(pas d'interface neuve — on change l'URL). `ORION_LSDP_MODE` (bespoke) suffit pour le
reste et reste indépendant du profil. `ORION_PROFILE` ne fait que **choisir les impls**
au boot — aucune branche dans le chemin chaud (D3).

### 3.4 — Cockpit (Question E)

Le cockpit est **host-agnostic** (ADR 008 : il lit `/cockpit/contracts`, résout via
`/operator/*`). Il pointe simplement sur l'Orion loopback en local vs l'Orion antenne
distant. **Aucun changement cockpit dû à cet ADR** — c'est précisément l'invariant D2.

## 4. Consequences

- Le mode local exécute le **vrai moteur**, pas une réimplémentation. `local ==
  antenne` tenu : un seul binaire, un seul chemin chaud, plomberie de bord pluggable.
- Orion gagne **2 nouvelles interfaces** (`Store`, `AuthSource`) + une 2ᵉ impl chacune ;
  le `Fetcher` gagne une 2ᵉ impl. Le profil antenne est **inchangé** (impls par défaut).
- Prism gagne un **sidecar Orion** (+ un sidecar data, ou son fixture-équivalent) et
  un **bundle de scène** (artefacts Canvas/Blue figés).
- Le contrat opérateur ADR 008 devient exploitable **en local** sans aucune adaptation
  → les deux boutons LCK/LEC (phase A) et le sélecteur de match (phase B) marchent en
  embarqué.
- Un **2ᵉ chemin store (SQLite)** existe : risque de divergence à surveiller — mitigé
  par des **tests de parité** store (RC-6) et par le fait que `_query`/fetch/auth
  restent iso-contrat.

## 5. Risks

- **R1 — Divergence `local ≠ antenne` (D2).** Le store SQLite est le seul chemin
  réellement dédoublé. Mitigation : tests de parité sur l'interface `Store` exécutés
  contre les **deux** impls (pg + sqlite) ; jsonb gardé opaque.
- **R2 — Sécurité du shim d'auth local (D4).** `localOperatorAuth` accorde `operator`
  sur loopback. Vecteurs : un autre process local POST sur le port et obtient operator ;
  fuite du port. Mitigations : **loopback-only strict**, **secret de handshake**
  Prism↔Orion, jamais d'exposition réseau, jamais sur l'antenne (gardé par
  `ORION_PROFILE`). **→ clearance Bastion requise** sur `localOperatorAuth` + le
  binding loopback avant merge du volet auth.
- **R3 — Contrat `_query` local non conforme.** Si le sidecar data dévie du wire
  ZabGate, les `db.query` divergent. Mitigation : **contrat figé avec Conduit**
  (réutiliser le wire `_query` existant, pas en réinventer un).
- **R4 — Poids du bundle / cross-compile.** Binaire Orion ×3 OS + miroirs SQLite.
  Mitigation : binaire Go statique léger ; miroirs réduits au périmètre canvas-chat-
  sponso pour le MVP.
- **R5 — Migrations SQLite désynchronisées du schéma pg.** Mitigation : générer/tester
  les deux jeux dans la même CI ; faire échouer la CI si une migration pg n'a pas son
  équivalent sqlite.

## 6. Resolution criteria

- **RC-1 (profil additif)** — `ORION_PROFILE` absent ou `antenne` ⇒ comportement
  **strictement inchangé** (pg + ZabGate headers + HTTP fetch). Prouvé par la suite
  antenne existante verte sans modification.
- **RC-2 (sidecar Prism)** — Prism lance/arrête proprement le sidecar Orion en
  loopback (lazy keep-alive, health, stop au quit), packagé multi-OS comme Pulsar.
- **RC-3 (store SQLite)** — interface `Store` extraite ; `sqliteStore` passe la **même
  suite de tests** que `pgStore` (parité), sur un fichier local. Aucune requête
  jsonb-spécifique sur le chemin chaud (ou couverte par `json1`).
- **RC-4 (auth local)** — `localOperatorAuth` accorde `operator` sur requête loopback
  authentifiée par handshake ; `requireOperator` fonctionne à l'identique ; refus
  hors loopback / sans handshake. **Clearance Bastion** sur ce point avant merge.
- **RC-5 (data + fetch locaux)** — `db.query` de la scène canvas-chat-sponso résout
  via le sidecar data loopback (contrat `_query` iso-ZabGate, validé Conduit) ; le
  `bundledFetcher` sert layout + blueprints figés ; la scène **compile et s'exécute
  en local**.
- **RC-6 (MVP phase A end-to-end)** — Prism ouvert en `embedded-local`, **les deux
  boutons cockpit LCK/LEC (`on-call`) exécutent la scène canvas-chat-sponso en local
  embarqué**, scores tirés du data local, rendu poussé à Solar — **zéro infra
  externe**.
- **RC-7 (MVP phase B)** — un `await-value` + recherche sur le **catalogue DB local**
  (ADR 008 §3.4 servi par le sidecar data) permet de choisir n'importe quel match en
  local.

## 7. Alternatives écartées (synthèse)

- **A1 — Réimplémenter un mini-moteur local en Node/TS dans Prism.** Rejeté frontalement
  par le porteur et par D2 : ce serait un 2ᵉ moteur non testé à l'antenne. On embarque
  le **vrai binaire**.
- **A2 — Orion in-process (Go compilé en lib appelée depuis Node).** Rejeté : pas
  précédenté, fragile, casse le modèle sidecar éprouvé (Pulsar).
- **A3 — embedded-postgres bundlé.** Rejeté (cf. §3.2-1) : poids/lifecycle contraires
  à D1/D5 ; SQLite suffit.
- **A4 — Garder Docker mais « one-click ».** Rejeté par la décision porteur ferme
  (perte sèche d'intérêt produit).
- **A5 — RFC/Thinker préalable.** Non nécessaire : décision porteur ferme, faisabilité
  haute (3 des 4 abstractions existent déjà), périmètre MVP net. Le seul débat utile
  (sécu du shim local) est ciblé → Bastion (R2/RC-4).

## 8. Découpage & séquence (issues consommables)

Légende : **[O]** Orion (abstraction/runtime) · **[P]** Prism (packaging) · **[D]**
data locale · **[C]** Conduit (contrat) · **[B]** clearance Bastion.

**Fondations (parallélisables) :**
- **#O-store-iface [O]** — extraire l'interface `Store` ; `pgStore` = impl défaut ;
  tests de parité paramétrés (RC-3, R1). *Sans dep.*
- **#O-auth-source [O]** — extraire `AuthSource` derrière `FromHeaders` ; `headerAuth`
  = défaut (RC-1). *Sans dep.*
- **#O-profile-flag [O]** — `ORION_PROFILE` (défaut `antenne`) câblant le choix des
  impls au boot ; loopback-only en `embedded-local` (RC-1). *Sans dep.*

**Phase A (MVP — deux boutons LCK/LEC) :**
- **#O-sqlite-store [O]** — `sqliteStore` + migrations SQLite, parité verte
  (RC-3). *Dep : #O-store-iface.*
- **#O-local-auth [O][B]** — `localOperatorAuth` (handshake loopback, operator), refus
  hors loopback (RC-4). *Dep : #O-auth-source, #O-profile-flag.* **Merge gated Bastion.**
- **#O-bundled-fetcher [O]** — `bundledFetcher` (layout + blueprints figés depuis un
  bundle de scène) (RC-5). *Dep : aucune côté interface (Fetcher existe).*
- **#D-data-sidecar [D][C]** — sidecar data loopback exposant `_query` iso-ZabGate sur
  miroirs/fixtures LCK/LEC (RC-5). *Dep : contrat figé Conduit (#C-query-contract).*
- **#C-query-contract [C]** — figer/valider que le `_query` local == wire ZabGate
  existant (R3). *Sans dep ; bloque #D-data-sidecar.*
- **#P-orion-sidecar [P]** — bundling binaire Orion multi-OS + lifecycle lazy
  keep-alive + health + loopback (RC-2). *Dep : #O-profile-flag.*
- **#P-scene-bundle [P]** — packager le bundle de scène canvas-chat-sponso (artefacts
  Canvas/Blue figés) consommé par `#O-bundled-fetcher` + `#D-data-sidecar`. *Dep :
  #O-bundled-fetcher.*
- **#A-mvp-e2e [P]** — câbler les deux boutons cockpit (`on-call` LCK/LEC, ADR 008) sur
  l'Orion loopback ; preuve end-to-end zéro-infra (RC-6). *Dep : tous les Phase A.*

> **Note** : `on-call`/`await-value` viennent d'ADR 008 (issues Blue #166/#167 +
> Orion #209). Le MVP local **consomme** ces primitives — il ne les ré-livre pas.

**Phase B (sélecteur de match libre) :**
- **#B-db-catalog-local [O/D]** — exposer le catalogue DB (ADR 008 §3.4 : `/db/{svc}/
  schema`) en local via le sidecar data (RC-7). *Dep : #D-data-sidecar, Orion #211.*
- **#B-await-match [P]** — flux cockpit `await-value` + recherche match en local (RC-7).
  *Dep : #B-db-catalog-local, Orion #209/#210.*

**Séquence MVP** : (#O-store-iface ∥ #O-auth-source ∥ #O-profile-flag ∥ #C-query-contract)
→ (#O-sqlite-store ∥ #O-local-auth[B] ∥ #O-bundled-fetcher ∥ #D-data-sidecar ∥
#P-orion-sidecar) → #P-scene-bundle → **#A-mvp-e2e (RC-6)**. Phase B ensuite.

## 9. Points à clearer (hors décision solo)

- **Bastion** : `localOperatorAuth` + handshake loopback + binding loopback-only
  (R2/RC-4) — surface d'auth d'un moteur qui accorde `operator`. Flag explicite.
- **Conduit** : contrat `_query` local == wire ZabGate (R3, #C-query-contract) ; et
  le format du **bundle de scène** (artefacts Canvas/Blue figés) si transverse.
- **Keeper** : cross-compile Go multi-OS en CI release + packaging electron-builder du
  sidecar (RC-2), aligné sur le pipeline Pulsar.

> `proposed → accepted` : **Vigil**. La clearance **Bastion** (R2/RC-4) conditionne le
> merge du volet auth local (#O-local-auth), pas l'acceptation de l'ADR.

---

## Amendment 1 — Scène arbitraire en conditions réelles : abandon du bundle figé (2026-06-22)

- **Author:** Atlas
- **Status:** accepted (Amendment) — `proposed → accepted` par Vigil le 2026-06-22
- **Renverse:** §3.2(4) (`bundledFetcher` / bundle de scène figé) + la **cible MVP
  « scène figée canvas-chat-sponso »** comme finalité. Le reste de l'ADR (sidecar Orion,
  store SQLite, auth loopback, sidecar data, profil additif) **reste valable**.
- **Conserve l'historique** : on n'efface pas §3.2(4) ni les RC-5/RC-6 d'origine — ils
  documentent l'étape figée déjà livrée (#224 `bundledFetcher`, #232 `SCENE_BUNDLE_PATH`,
  Prism #150/#151). Cet amendement **déplace la cible** au-delà.

### A1.1 — Contexte : pourquoi le figé est un mur

La cible d'origine (§1, §3.5, RC-6) était d'exécuter **une** scène (canvas-chat-sponso)
bakée dans un `scene-bundle.json`, servie par `BundledFetcher` à partir d'un
`ORION_SCENE_BUNDLE_PATH` **requis au boot** (`config.go` fait échouer le boot
embedded-local sans lui). C'est livré et ça prouve le chemin chaud local == antenne
**pour une scène**.

**Décision porteur (2026-06-22, non négociable) :** la preview locale du cockpit Prism
doit reproduire **totalement** l'antenne pour **n'importe quelle scène** que l'opérateur
sélectionne, **dans les conditions réelles** — pas une scène pré-bakée. Tout contournement
côté Prism (re-push pour armer l'exec d'une scène arbitraire par-dessus le bundle figé)
est du rafistolage **rejeté**. Le `bundledFetcher` ne peut servir qu'**un** jeu d'artefacts
gelé : il est structurellement incapable du scene-agnostic.

**Constat d'archi (vérifié sur `origin/main`) :** le chemin scène-arbitraire de l'antenne
**existe déjà et n'a rien de spécial** — c'est `POST /api/v1/scenes/{id}/push` (envelope →
`compiler.Compile(ctx, id, envelope, deps.Fetcher)` → `Store` pushed version) puis
`POST /api/v1/show/active-scene` (gate validation `isAirEligible` → `Show.SetActive`).
Le seul composant qui « connaît » les scènes est le **`Fetcher`**, et l'antenne le câble
sur **`httpFetcher`** (`NewHTTPFetcherWithTokenFunc(CanvasBaseURL, BlueBaseURL, …)`) qui
GET `/<canvas>/api/v1/layouts/{v}`, `/<blue>/api/v1/blueprints/{id}/…`,
`/_compute-manifest`. **Tous ces champs config (`CanvasBaseURL`, `BlueBaseURL`,
`ZabGateURL`) existent déjà dans `config.go` en embedded-local** — ils sont juste
court-circuités par la branche `IsEmbeddedLocal()` de `main.go` qui force le
`bundledFetcher`.

⇒ **Le scene-agnostic en local ne demande PAS un nouveau mécanisme.** Il demande que
embedded-local **réutilise le `httpFetcher`** pointé sur une **gateway loopback locale**,
exactement comme l'antenne pointe sur ZabGate. C'est la suite logique de l'invariant D2
(`local == antenne`) que le bundle figé **violait partiellement** (chemin fetch dédoublé,
non exercé à l'antenne).

### A1.2 — Décision : `gatewayFetcher` loopback, abandon du bundle figé requis

**GO.** En embedded-local, le `Fetcher` redevient le **`httpFetcher`** (le chemin antenne,
inchangé), pointé sur une **gateway loopback bundlée dans Prism** qui sert les surfaces
`/canvas/*` et `/blue/*` (et `/<svc>/api/v1/_query` déjà prévu §3.2(3)). Le profil
embedded-local cesse d'exiger `ORION_SCENE_BUNDLE_PATH` ; le `bundledFetcher` est conservé
comme **mode dégradé optionnel** (offline / smoke) mais n'est plus le chemin nominal ni
requis.

**Arbitrage clé — « gateway locale » ≠ « stack microservices Docker ».** C'est la tension
réelle de cet amendement, tranchée franchement :

- **Le porteur interdit Docker / la stack microservices côté client** (D1). Lever
  ZabCanvas+Blue+ZabGate complets en local **rétablirait** cette stack — **rejeté**, comme
  l'ADR le disait déjà en écartant « embarquer Blue/ZabCanvas complets ».
- **Mais** servir les surfaces `/canvas` et `/blue` **ne requiert pas les services
  d'authoring** : ce sont des surfaces de **lecture d'artefacts publiés** (layouts par
  version, blueprints par version pinnée, compute-manifest, components). Le **sidecar data
  loopback déjà décidé (§3.2(3))** est étendu pour servir ces routes depuis des **miroirs
  locaux** (SQLite/fichiers) des artefacts publiés — **un seul sidecar Go bundlé**, zéro
  Docker, zéro Python d'authoring.
- **Modèle cible :** Prism bundle **un sidecar « gateway loopback »** (extension du sidecar
  data) qui réunit, derrière une base loopback unique (`ORION_ZABGATE_URL` /
  `ORION_CANVAS_BASE_URL` / `ORION_BLUE_BASE_URL` pointés sur lui) :
  1. `GET /canvas/api/v1/layouts/{version}` + `/components/{id}/{v}` — miroir des layouts
     publiés ZabCanvas ;
  2. `GET /blue/api/v1/blueprints/{id}`, `/versions/{v}`, `/versions/{v}/graph`,
     `/_compute-manifest` — miroir des blueprints/graphes publiés Blue ;
  3. `POST /<svc>/api/v1/_query` — déjà §3.2(3), inchangé.
  Orion en embedded-local utilise alors **strictement le chemin antenne** : `httpFetcher`
  + `pushScene` + `postActiveScene` + boucle d'exec — **aucune branche moteur** propre au
  local au-delà du choix d'impl de bord au boot.

**Pourquoi c'est conforme D1 ET D2 :** D1 (zéro infra client) tenu car c'est **un binaire
Go loopback** bundlé comme Pulsar, pas une stack ; D2 (`local == antenne`) **renforcé** —
on supprime le seul chemin fetch dédoublé (le `bundledFetcher`) au profit du chemin
antenne réel. L'invariant byte-for-byte passe du seul `_query` à **fetch + push + compile
+ exec** entiers.

### A1.3 — Provenance des artefacts : miroirs vs services (arbitrage `_query` data live)

Le moteur ne change pas ; reste à **alimenter** la gateway loopback. Trois questions, trois
tranches :

1. **Artefacts de scène (layouts/blueprints publiés)** — le sidecar gateway sert des
   **miroirs locaux** des versions publiées. Seed = **export depuis ZabCanvas/Blue**
   (snapshot des artefacts publiés) materialisé dans le store local du sidecar.
   **Provenance du miroir = en ligne au moment où l'opérateur ouvre Prism connecté**, ou
   pré-seedé au packaging. **Tranche :** pour la **parité conditions réelles**, le miroir
   doit pouvoir être **rafraîchi à la demande** (sync des scènes publiées que l'opérateur
   est autorisé à voir) — sinon « n'importe quelle scène » se limite aux scènes seedées.
   Le **mécanisme de sync** (pull authentifié ZabCanvas/Blue → miroir local) est une issue
   dédiée (#A1-mirror-sync), **distincte** de l'exécution. Hors-ligne total = mode dégradé
   sur le dernier miroir.
2. **Données live `_query` (ZabTruth/ZabRanking)** — **inchangé §3.2(3)** : sidecar
   `_query` iso-contrat sur miroirs SQLite. **Tranche sur le « réel » :** en preview,
   `_query` sur miroir local **est** la condition réelle acceptable (la doctrine porteur
   `local == antenne` porte sur le **chemin d'exécution**, pas sur la fraîcheur seconde des
   données match) ; un refresh live optionnel du miroir suit le même #A1-mirror-sync. La
   fraîcheur temps-réel des données match n'est **pas** un invariant de cet ADR.
3. **Auth opérateur** — **inchangé §3.2(2)** : `localOperatorAuth` loopback + handshake.
   `pushScene`/`postActiveScene`/`operator/*` sont tous derrière `requireOperator`, qui
   fonctionne à l'identique sous le shim local (clearance Bastion R2/RC-4 toujours requise).

### A1.4 — Conséquences sur les changements & issues

- **Orion** — (a) `main.go` : en embedded-local, câbler le **`httpFetcher`** sur les
  base-URLs loopback au lieu du `bundledFetcher` ; (b) `config.go` : **`SCENE_BUNDLE_PATH`
  cesse d'être requis** en embedded-local (rendu optionnel / mode dégradé) ; le boot exige
  désormais `ORION_CANVAS_BASE_URL`+`ORION_BLUE_BASE_URL`+`ORION_ZABGATE_URL` loopback ;
  (c) la **validation gate** `isAirEligible` (ADR 003) doit avoir un chemin local : soit le
  miroir importe l'enregistrement `validated`, soit embedded-local **valide à la volée** via
  le harness local au push (à trancher en issue, défaut = importer le `validated` du miroir
  pour rester iso-antenne).
- **Prism** — le sidecar data (#225) devient le **sidecar gateway loopback** (ajoute
  `/canvas/*` + `/blue/*`) ; orion-engine pointe les 3 base-URLs sur lui et **cesse de
  passer `ORION_SCENE_BUNDLE_PATH`** (ou le passe en fallback) ; le cockpit pousse
  `push` + `active-scene` quand l'opérateur **sélectionne une scène** (aujourd'hui il
  exécute la scène bakée) ; +UI de sélection de scène (liste des scènes du miroir).
- **ZabCanvas / Blue** — fournir/figer un **export d'artefacts publiés** consommable par le
  seed/sync du miroir (read-only des versions publiées). Pas de nouveau service.
- **Conduit** — étendre le contrat embedded-local (`docs/contracts/embedded-local-contracts.md`)
  aux surfaces `/canvas` + `/blue` loopback (iso-réponses ZabCanvas/Blue, comme `_query` est
  déjà iso-ZabGate).
- **Bastion** — surface inchangée sur l'auth (R2) ; **nouveau point** : le mécanisme de sync
  miroir (#A1-mirror-sync) authentifie un pull d'artefacts — vérifier qu'il ne fuit pas de
  scènes hors périmètre opérateur et reste loopback côté Orion.

### A1.5 — Issues (amendement) — ordre de dépendance

- **#A1-fetcher-loopback [O]** — en embedded-local, câbler `httpFetcher` sur base-URLs
  loopback ; rendre `SCENE_BUNDLE_PATH` optionnel (fallback dégradé) ; exiger les 3
  base-URLs loopback au boot. *Dep : profil #O-profile-flag (déjà livré).* **RC-A1**
- **#A1-gateway-sidecar [P/D]** — étendre le sidecar data Prism aux routes `/canvas/*` +
  `/blue/*` (miroirs d'artefacts publiés), iso-réponses ZabCanvas/Blue. *Dep :
  #A1-canvas-blue-contract.* **RC-A2**
- **#A1-canvas-blue-contract [C]** — Conduit fige les réponses `/canvas` + `/blue` loopback
  == wire ZabCanvas/Blue (le `httpFetcher` ne doit voir aucune différence). *Sans dep ;
  bloque #A1-gateway-sidecar.*
- **#A1-mirror-seed [D/ZabCanvas/Blue]** — export des artefacts publiés (layouts +
  blueprints + graphes + compute-manifest + enregistrements `validated`) vers le miroir
  local ; seed au packaging. *Dep : #A1-canvas-blue-contract.* **RC-A3**
- **#A1-mirror-sync [P/D/B]** — refresh à la demande du miroir (pull authentifié des scènes
  publiées autorisées), pour le scene-agnostic réel au-delà du seed. *Dep : #A1-mirror-seed.*
  **Clearance Bastion** (périmètre opérateur, no-leak). **RC-A4**
- **#A1-validation-local [O]** — chemin de la gate `isAirEligible` en local (import du
  `validated` du miroir, défaut iso-antenne ; ou validation locale au push — trancher).
  *Dep : #A1-mirror-seed.* **RC-A5**
- **#A1-prism-scene-select [P]** — UI de sélection de scène (liste du miroir) → cockpit
  `push` + `active-scene` sur l'Orion loopback à la sélection. *Dep : #A1-fetcher-loopback,
  #A1-gateway-sidecar, #A1-mirror-seed.* **RC-A6**
- **#A1-e2e-any-scene [P]** — preuve : opérateur ouvre Prism, **sélectionne une scène
  arbitraire**, l'aperçu fait tourner le vrai Solar sur l'Orion loopback, `push`+activate
  compile la scène via le `httpFetcher` loopback, les blueprints exécutent (triggers
  cockpit + `_query`), les deltas peignent en temps réel — **zéro infra externe, parité
  antenne**. *Dep : tous les #A1.* **RC-A7 (critère de « fini » de l'amendement)**

**Séquence :** #A1-canvas-blue-contract → (#A1-fetcher-loopback ∥ #A1-mirror-seed) →
#A1-gateway-sidecar → (#A1-validation-local ∥ #A1-mirror-sync[B]) → #A1-prism-scene-select
→ **#A1-e2e-any-scene (RC-A7)**.

### A1.6 — Resolution criteria (amendement, testables)

- **RC-A1** — En embedded-local, le `Fetcher` actif est le `httpFetcher` (pas le
  `bundledFetcher`) ; le boot **réussit sans** `ORION_SCENE_BUNDLE_PATH` et **échoue** si
  une des 3 base-URLs loopback manque. Le profil antenne reste byte-for-byte inchangé (RC-1
  d'origine toujours vert).
- **RC-A2** — Le sidecar gateway loopback répond aux GET `/canvas/api/v1/layouts/{v}`,
  `/blue/api/v1/blueprints/{id}/versions/{v}/graph`, `/_compute-manifest` avec des corps
  byte-identiques aux réponses ZabCanvas/Blue (golden vs capture, validé Conduit).
- **RC-A3** — Le miroir seedé contient ≥ 2 scènes publiées distinctes ; chacune compile via
  le chemin `pushScene` local sans erreur.
- **RC-A4** — Un refresh miroir importe une scène publiée non présente au seed et la rend
  sélectionnable, sans exposer de scène hors périmètre opérateur (clearance Bastion).
- **RC-A5** — `postActiveScene` local applique la même gate `isAirEligible` que l'antenne
  (une scène non-validée est refusée `SCENE_NOT_VALIDATED`).
- **RC-A6** — Dans le cockpit, sélectionner une scène B alors que A est active déclenche
  `push`(B)+`active-scene`(B) sur l'Orion loopback et bascule l'aperçu (parité ADR 008
  active-only : seule B exécute).
- **RC-A7 (fini)** — Prism ouvert, l'opérateur sélectionne **une scène arbitraire du
  miroir** (≠ canvas-chat-sponso), l'aperçu = vrai Solar sur Orion loopback, les blueprints
  s'exécutent en conditions réelles (triggers cockpit `on-call`/`await-value` + `_query`
  data) et les deltas peignent en temps réel — **zéro infra externe**. Mesuré comme RC-6
  d'origine, mais sur une scène **choisie**, pas bakée.

### A1.7 — Invariants (rappel après amendement)

- `local == antenne` **renforcé** : même `httpFetcher`, même `pushScene`/`postActiveScene`,
  même boucle d'exec ; la divergence se réduit au **branchement de bord** (base-URLs
  loopback + store SQLite + auth shim), aucun chemin moteur forké.
- **Active-only (ADR 008)** préservé : une seule scène exécute en preview, comme à l'antenne.
- **Sécurité de bord (D4)** : Orion loopback-only, `localOperatorAuth` + handshake ; le
  sidecar gateway loopback-only ; le pull de sync (seul flux sortant) authentifié et borné
  au périmètre opérateur (Bastion).
- **Bundle figé** : conservé comme **fallback offline/smoke**, plus jamais le chemin nominal.
