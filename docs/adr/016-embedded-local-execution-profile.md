# ADR 016 — Profil d'exécution `embedded-local` : Orion embarqué dans Prism

- **Status:** proposed
- **Date:** 2026-06-21
- **Decided:** —
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
