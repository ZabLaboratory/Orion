# ADR 018 — Postgres en CI sur le substrat runner self-hosted Zab

- **Status**: accepted
- **Date**: 2026-08-11
- **Decided**: 2026-08-11
- **Deciders**: @ClodoCapeo
- **Author**: Atlas
- **Supersedes**: —
- **Superseded by**: —

---

> **Portée — à lire avant le §1.** Cet ADR vit dans le dépôt Orion parce que le
> protocole ADR exige un dépôt git (commit signé, PR, persistance Vigil) et que
> l'étage 0 n'en est pas un, et parce qu'Orion porte déjà l'implémentation de
> référence citée ici (`.github/workflows/ci.yml:64-137`). **Son autorité n'est pas
> celle d'un ADR runtime Orion** : elle s'étend au substrat CI partagé des ~18 repos
> de l'org ZabLaboratory. Le numéro 018 appartient à l'espace de numérotation d'Orion
> et ne signifie pas que la décision est locale à Orion : tout repo du pool est lié
> par les interdits du §3 et peut adopter le canon du §3.1 sans ADR supplémentaire.

## 1. Context

Depuis le gel définitif de la facturation GitHub Actions de l'org ZabLaboratory
(décision porteur 2026-06-20), toute la CI des ~18 repos Zab s'exécute sur le pool
self-hosted `vps-ovh`. Ce pool est en docker-in-docker : les jobs tournent dans un
conteneur runner éphémère (couche statique `/home/ubuntu/zab-org-runners`, couche JIT
`/home/ubuntu/runner-orchestrator-zab`) **sans le socket Docker de l'hôte monté**.
Conséquence directe : le bloc `services:` de GitHub Actions — qui exige que le runner
crée lui-même des conteneurs — est **structurellement inutilisable**.

Preuve fraîche (Keeper, unité B20-40, issue ZabLaboratory/Blue#212) : l'ajout d'un job
`services: postgres:16` a échoué à l'init container avec « Cannot connect to the Docker
daemon » — PR ZabLaboratory/Blue#222, run 31443498002, revert `4b464d6`, PR fermée,
branche supprimée, rien laissé en place. Keeper a produit la preuve de migration/test
réelle **hors CI**, par des conteneurs éphémères lancés en SSH sur l'hôte (40 tests verts
contre un vrai Postgres 16, round-trip `alembic upgrade head` / `downgrade base` deux
fois). C'est une preuve valide **une fois**, pas une gate : toute PR future touchant
`blue_server` ou `alembic_blue_server` retomberait sur une vérification manuelle, ce qui
contredit frontalement `docs/rules/git.md` (« la gate de merge est non négociable ») et
l'invariant d'ADR 004 (« un job qui doit bloquer doit bloquer »).

Le constat central de cet ADR est cependant que **le problème est déjà résolu ailleurs
dans le fleet, et non détecté comme tel** :

- l'image du pool n'est pas `myoung34/github-runner` vanilla mais **`zab-org-runner:pg`**,
  dérivée avec `postgresql`, `postgresql-client` et un sudoers NOPASSWD pour `runner`
  (bascule 2026-06-21, GO porteur ; build local `docker build --load`, compose
  `pull_policy: never`) ;
- l'orchestrateur JIT spawne également sur `RUNNER_IMAGE=zab-org-runner:pg` (correctif
  Keeper 2026-06-28 : le JIT spawnait auparavant l'image vanilla, trou latent org-wide) ;
- **Orion exerce ce chemin en CI en continu** : `Orion/.github/workflows/ci.yml:64-137`,
  job `e2e (Postgres)`, démarre un cluster PG *dans* le conteneur runner (shim de
  privilège, branche skip-apt quand PG est pré-provisionné, `pg_ctlcluster … start ||
  restart`, attente `pg_isready`, bootstrap SQL idempotent rôle+DB), puis
  `go test -tags e2e` contre `postgres://…@localhost:5432/…`. L'en-tête du fichier
  (`ci.yml:17-19`) énonce déjà l'invariant : « No Docker daemon in the runner: the `e2e`
  job uses PostgreSQL natively (no `services: postgres` container is possible without a
  Docker socket) ».

Deux angles morts subsistent, qui sont l'objet réel de la décision :

1. **Version non épinglée — et la version réellement servie est PG 12.** L'image
   installe le méta-paquet `postgresql` via apt sans contrainte de version : elle hérite
   du dépôt de la distribution de base. **Fait établi** (Bastion, work unit
   `ZAB-CI-POSTGRES-NATIVE-THREATMODEL-20260811`, 2026-08-11) : le pool sert
   **PostgreSQL 12**, pas 16 — preuve run Orion `31309561765`, job `e2e (Postgres)`,
   ligne `PostgreSQL already provisioned (12 main) — skipping apt`, cohérente avec la base
   **Ubuntu 20.04** de `myoung34/github-runner`. Or Blue, ZabCanvas,
   Quasar, Cosmos, ZabAuth
   contractualisent **PostgreSQL 16** (`agents/_shared/conventions.md`), et Orion tourne
   PG 16 en prod. Une CI qui valide des migrations Alembic sur une version différente de
   la prod est une gate qui ment.

   **Corollaire non trivial : Orion est en dérive active depuis l'origine.** Son job
   `e2e (Postgres)` s'exécute contre PG 12 alors que sa prod tourne en PG 16 — la seule
   suite e2e DB du fleet valide donc, aujourd'hui, un moteur que personne ne déploie.
   Aucune régression n'en a encore résulté, mais l'écart couvre quatre versions majeures
   de comportement (types, planificateur, syntaxe). Cet ADR ferme cette dérive comme
   effet de bord de l'épinglage, ce qui en fait un bénéfice immédiat pour un repo qui
   n'avait rien demandé.
2. **Substrat hors git.** Le `Dockerfile` de `zab-org-runner:pg`
   (`/home/ubuntu/zab-org-runners/image/`) et le `.env` de l'orchestrateur
   (`/home/ubuntu/runner-orchestrator-zab/.env`, 0600) sont **hand-managed sur le VPS,
   non versionnés**. Le correctif `RUNNER_IMAGE` de 2026-06-28 tient sur une ligne d'un
   fichier que rien ne protège. Un rebuild depuis l'upstream de l'orchestrateur écrase
   les patchs locaux (c'est déjà arrivé pour `RUNNER_MEM_OVERRIDES`). L'invariant « PG
   est disponible dans le runner » n'a aujourd'hui **aucun garde-fou mécanique**.

Note de découvrabilité : il n'existe aujourd'hui **aucun canon CI Zab écrit** pour le cas
DB. L'ADR 004 de l'étage 0 (`docs/adr/004-cicd-skeleton-etage2.md`) est scopé G2 par son
titre et son §1 ; sa prescription `docker run -p :5432` ne s'applique pas ici et **n'a pas
à être corrigée** — mais rien, en face, n'énonçait le canon Zab. C'est ce **vide**, et non
une prescription erronée, qui a coûté la PR #222. Comblé par RC 13.

## 2. Decision drivers

- **Gate réellement enforçable.** Une migration Alembic non exercée en CI est une
  régression prod en puissance ; la vérification manuelle n'est pas une gate.
- **Surface d'attaque du runner partagé.** Le pool est org-scoped : il sert 18 repos.
  Toute permission accordée au conteneur runner est accordée à la CI de tous les repos.
  `vps-ovh` héberge **aussi la prod** (services `zab-internal` et leurs volumes
  Postgres) : le démon Docker y est une clé de la plateforme entière, pas une commodité
  de CI.
- **Ne pas réinventer ce qui tourne.** Le pattern natif est en production CI depuis
  ~7 semaines sur Orion, avec les pièges déjà rencontrés et documentés inline (privilège
  variable, lock apt, résidus de cluster).
- **Capacité du pool.** 3 runners éphémères, `RUNNER_MAX_CONCURRENCY=3`, `RUNNER_CPUS=2`,
  `RUNNER_MEM=1g` global (override `zablaboratory/prism=2g`) → ~6 jobs concurrents pour
  18 repos, contention déjà structurelle. Toute solution qui ajoute des jobs ou de la
  mémoire par job doit se justifier.
- **Fidélité à la prod.** Version PG identique à la prod, sinon la gate valide un
  artefact que personne ne déploiera.
- **Résistance au drift.** Le substrat est hand-managed ; une décision qui repose sur un
  état VPS non versionné a une durée de vie inconnue.

## 3. Decision

**Le canon Zab pour toute CI ayant besoin d'une base de données est : PostgreSQL démarré
nativement dans le conteneur runner. Le bloc `services:` de GitHub Actions est déclaré
inutilisable sur le substrat `vps-ovh` et interdit dans les workflows Zab. Aucun accès au
démon Docker de l'hôte — direct ou proxifié — n'est accordé aux conteneurs runner.**

### 3.1 Forme canonique du job (contrat)

Tout job CI Zab nécessitant Postgres porte un step préalable conforme au patron éprouvé
d'`Orion/.github/workflows/ci.yml:73-137`, dont les invariants sont :

- **shim de privilège** résolu une fois (`id -u` = 0 → préfixe vide, sinon `sudo -n` s'il
  existe) — l'utilisateur effectif du runner a varié d'un run à l'autre, une commande
  `sudo` inconditionnelle est un flake ;
- **branche skip-apt** : `apt-get` n'est exécuté **que** si `pg_ctlcluster` est absent,
  avec retry borné sur le lock ; sur image pré-provisionnée le step est un no-op réseau.
  Un job dont la seule voie verte passe par un `apt-get install` runtime est non conforme ;
- **démarrage idempotent** : `pg_ctlcluster "$PGVER" main start || … restart`, puis
  attente `pg_isready` bornée ;
- **bootstrap idempotent** rôle + base, staged en fichier SQL puis exécuté sous
  l'utilisateur OS `postgres` (jamais un heredoc imbriqué dans `su -c`), qui
  `DROP DATABASE IF EXISTS` avant recréation pour partir d'un état connu malgré
  d'éventuels résidus d'image ;
- **garde fork obligatoire** : le job porte
  `if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository`
  (`Orion/.github/workflows/ci.yml:34,67`). Sans lui, une PR ouverte depuis un fork
  exécute du code arbitraire sur un runner du pool partagé — R4bis de Bastion a établi
  qu'au moins un repo **public** du parc est dans ce cas. Cet invariant vaut pour **tout**
  job tournant sur le substrat, pas seulement les jobs DB : un repo qui adopterait le
  canon §3.1 verbatim sans lui hériterait de l'exposition ;
- **URL de connexion en `env:` du job**, jamais un secret : les credentials de la base CI
  sont locaux au conteneur, éphémères, et sans valeur hors du job (`orion_e2e_ci` en est
  le précédent). Ils ne doivent en aucun cas être des GitHub Secrets — un secret de CI
  est un secret à faire fuiter.

Pour Blue, le raccordement est `BLUE_SERVER_TEST_POSTGRES_URL` pointant sur
`localhost:5432`, la variable que Keeper a déjà exercée hors CI pour obtenir ses 40 tests
verts, dont `tests/blue_server/test_migrations.py` qui était jusqu'ici skippé en CI
(finding Probe sur B20-12).

### 3.2 Épinglage de version — PostgreSQL 16 via PGDG, obligatoire

L'image `zab-org-runner:pg` **doit servir PostgreSQL 16**, épinglé **explicitement dans le
`Dockerfile`**. Ce n'est pas une mise en conformité formelle : le pool sert aujourd'hui
**PG 12** (§1, preuve run `31309561765`), soit quatre versions majeures d'écart avec la
prod de tous les services Zab.

Il n'existe donc **pas** de branche « la version est peut-être déjà bonne ». L'unique
chemin est :

1. **Ajouter le dépôt PGDG** (`apt.postgresql.org`) dans
   `zab-org-runners/image/Dockerfile` et installer `postgresql-16 postgresql-client-16`
   — le dépôt de la base Ubuntu 20.04 ne propose pas PG 16, un simple
   `apt-get install postgresql-16` sans PGDG échouerait.
2. **Rebuild** `docker build --load` (jamais buildx-only, sinon l'image n'atterrit pas
   dans `docker images`), compose en `pull_policy: never` (image locale, pas de registry).
3. **Vérifier** `docker run --rm --entrypoint pg_lsclusters zab-org-runner:pg -h` : la
   version majeure retournée est `16`. L'`--entrypoint` est nécessaire — l'image runner a
   un point d'entrée propre qui, sans cette option, ignorerait la commande passée.

Le job CI dérive `PGVER` de `pg_lsclusters` (comme Orion) et **échoue explicitement** si
la version majeure n'est pas 16 — un contrôle d'une ligne qui transforme un futur drift
silencieux en échec CI lisible, et qui aurait rendu la dérive PG 12 visible dès son
apparition au lieu de la laisser vivre jusqu'à un audit.

> **Divergence assumée vis-à-vis d'une recommandation Bastion.** Bastion suggérait
> d'épingler la version *dans les workflows* plutôt que de l'auto-détecter. Le choix retenu
> ici — auto-détection via `pg_lsclusters` **plus** assertion d'échec — est délibéré, pas un
> oubli : il évite de dupliquer une constante de version dans N workflows (donc N endroits à
> rebumper, avec la dérive garantie que cela produit) tout en conservant exactement le même
> pouvoir de détection, puisque c'est l'assertion, et non l'épinglage, qui fait échouer le
> job sur une mauvaise version.

**Cohabitation des versions.** L'installation de PG 16 via PGDG laisse le cluster PG 12
existant en place (paquets Debian versionnés, clusters distincts). Le `Dockerfile`
**désinstalle explicitement** les paquets PG 12 ou, à défaut, supprime le cluster
`12/main`, de sorte que `pg_lsclusters | awk 'NR==1'` — l'expression exacte qu'utilisent
les jobs — ne puisse pas retomber sur le cluster hérité. Un `pg_lsclusters` à deux lignes
serait le mode d'échec silencieux le plus probable de cette migration.

### 3.3 Versionnement du substrat runner

Le contexte de build de l'image runner (`Dockerfile` + script de build) et la
configuration de l'orchestrateur JIT qui en dépend (`RUNNER_IMAGE`,
`RUNNER_SERVED_REPOS`, `RUNNER_MEM_OVERRIDES`) sont **sortis du régime hand-managed et
versionnés dans un repo Zab**, avec un `.env.example` documentant chaque clé et le `.env`
réel restant à l'étage 1 / sur le VPS. Sans cela, la présente décision n'est pas durable :
elle repose sur des fichiers qu'un `docker compose` upstream écrase et qu'aucune review ne
voit passer. Le repo d'accueil est tranché par Keeper à l'implémentation (repo dédié
`zab-ci-runners`, ou dossier versionné dans un repo infra existant) ; l'exigence porte sur
le versionnement, pas sur l'emplacement.

**Ce versionnement crée une surface sensible neuve, et elle est traitée comme telle
(R-SEC-3).** Le repo d'accueil devient le point de contrôle de l'image runner des 18
repos, sur un hôte qui porte aussi la prod : quiconque peut y pousser peut, à terme,
influencer ce qui s'exécute dans chaque job CI de l'org. La décision est donc **assortie
d'un régime d'accès obligatoire**, à satisfaire **avant** que le repo devienne la source
de vérité du build (et non après) :

- dépôt **privé**, `CODEOWNERS` couvrant la racine et le `Dockerfile`, revue obligatoire
  d'un owner ;
- **branch protection** sur `main` : pas de push direct, pas de force-push, PR requise ;
- **le rebuild et la mise en service de l'image restent un acte Keeper sur le VPS**, jamais
  un pipeline automatique déclenché par un push — un push ne doit pas suffire à changer
  l'image qu'exécutent 18 CI ;
- **clearance Bastion** sur ce régime d'accès avant bascule, distincte de son veto socket :
  son threat model actuel est scopé au socket Docker, pas à ce nouveau point de contrôle.

Le versionnement reste la bonne décision — l'alternative est le statu quo hand-managed,
où la même surface existe déjà mais **sans** revue, sans historique et sans CODEOWNERS.
On ne crée pas un pouvoir : on rend visible et contrôlable un pouvoir qui s'exerce
aujourd'hui à l'aveugle.

### 3.4 Budget de ressources

Un cluster PG dans le runner partage le plafond mémoire du conteneur (`RUNNER_MEM=1g`
global). PG 16 au repos tient largement, mais la conjonction PG + `uv sync` + pytest sur
un repo Python chargé peut friser le plafond. Le mécanisme d'override par repo existe
déjà (`RUNNER_MEM_OVERRIDES`, précédent `prism=2g`) : il est le levier prescrit, **repo
par repo, sur preuve d'OOM**, jamais un relèvement global qui réduirait la concurrence du
pool. Le canon **n'ouvre aucun job supplémentaire dédié au démarrage de la base** : le cluster
démarre dans un step du job qui en a besoin (chez Orion, le job `e2e (Postgres)` existant,
distinct de `build` — `ci.yml:31` vs `:64`). La contention du pool (≈6 slots pour 18 repos)
interdit d'ajouter des jobs gratuitement.

### 3.5 Portée — substrat global, adoption incrémentale

La décision se scinde en deux portées distinctes, et c'est délibéré :

- **Le volet substrat est nécessairement global et immédiat.** Il n'existe qu'une image
  de runner pour les 18 repos : épingler PG 16 (§3.2) et versionner le contexte de build
  (§3.3) sont des actes uniques qui bénéficient à tout le pool. Ils ne peuvent pas être
  « faits pour Blue seulement ».
- **Le volet consommateur est opt-in, repo par repo, à la demande réelle.** Aucune
  campagne d'alignement des 18 `ci.yml` n'est ouverte. Un repo adopte le step PG natif
  **quand il a effectivement un test qui exige une vraie base** — Orion l'a déjà, Blue le
  prend maintenant, les autres le prendront le jour où ils en auront besoin. Motif : un
  big-bang sur 18 repos consommerait la capacité d'un pool déjà en contention pour
  démarrer des clusters PG que la plupart des jobs n'utiliseraient pas, et ADR 004 §3.8
  a déjà établi que le rollout traçable et graduel bat le big-bang.

Ce qui est **global et opposable dès l'acceptation**, indépendamment de l'adoption : les
deux interdits de §3 (`services:` et tout accès au démon Docker) valent pour **tous** les
workflows Zab, y compris ceux qui n'ont pas de base de données. Ils sont vérifiables par
grep (§6) et ne coûtent rien à un repo qui ne les enfreint pas.

### 3.6 Options écartées

- **B — Monter `/var/run/docker.sock` (ou un socket-proxy) dans les conteneurs runner.**
  **NO-GO ferme — verdict Bastion rendu, contrainte, pas une préférence.** Motifs
  retenus : (a) la chaîne de compromission part du runner et va jusqu'à la plateforme
  entière — clé de l'App runner org-wide, `JWT_SECRET` partagé ZabGate/ZabAuth, volumes
  Postgres de **tous** les services, puisque le même démon Docker gouverne la CI et la
  prod ; (b) le repli « proxy filtrant » de `security.md` est **structurellement
  inapplicable ici** : `services: postgres` exige `POST /containers/create`, qu'un proxy
  catégoriel (GET-only / par verbe et route) ne peut pas filtrer sur le **corps** de la
  requête — or c'est le corps qui porte les bind-mounts, donc l'évasion. Élargir le
  `zab-runner-socket-proxy` existant (surface minimale, `IMAGES:0`, dédié à
  l'orchestrateur) annulerait le durcissement qu'il matérialise. Cette option est
  **retirée de l'espace de solutions** ; elle n'a pas à être réévaluée à chaque incident
  CI, la voie sûre atteignant le même résultat fonctionnel à surface constante.
- **C — Postgres partagé dédié CI, joignable sur le réseau des runners** (`ci-postgres`
  sur `runner-orchestrator-zab_zab-runner-net`, une base éphémère par run). *Écarté pour
  v1, conservé comme voie de sortie.* Crédible techniquement — les runners sont déjà
  attachés à ce bridge — et sans exposition Docker. Mais : (a) il introduit un credential
  partagé lisible par la CI de tout repo, donc un vecteur de pollution croisée entre
  repos ; (b) il crée un **SPOF pour toute la CI DB du fleet**, sur un réseau dont on sait
  qu'il disparaît au `docker prune` (panne « create 201 → start 404 » déjà vécue deux
  fois) ; (c) il impose une version PG unique à tous les repos ; (d) il ajoute un service
  persistant à opérer sur un VPS dont la saturation disque est un incident récurrent.
  *Condition de réévaluation* : si un repo a besoin d'une version PG différente de 16, ou
  si l'empreinte mémoire du PG in-runner devient le facteur limitant du pool.
- **D — Sidecar dockerd (DinD privilégié) par runner JIT.** *Écarté* : cumule le risque de
  B (conteneur privilégié) et un surcoût d'orchestration, pour un bénéfice nul face à A.
  Couvert par le même NO-GO Bastion.
- **E — Statu quo : vérification manuelle hors CI.** *Écarté comme état durable.* C'est
  précisément une gate non enforcée ; acceptable comme preuve ponctuelle (ce qu'a fait
  Keeper sur B20-40), inacceptable comme régime.

### 3.7 Frontières d'agent

Atlas conçoit (cet ADR). **Keeper** est le seul applicateur du substrat (image, VPS,
orchestrateur), l'auteur du relevé de version (§3.2, vérification `--entrypoint pg_lsclusters`) et des
PR `ci.yml`.
**Bastion** a rendu le verdict NO-GO sur l'exposition Docker ; il conserve son veto si
une implémentation s'en écartait. **Vigil** accepte l'ADR. **Scribe** inscrit le canon Zab dans `agents/_shared/conventions.md` (RC 13) ; il ne
modifie pas l'ADR 004 de l'étage 0, scopé G2. Atlas n'écrit aucun workflow.

## 4. Consequences

**Positives**

- La CI de `blue_server`/`alembic_blue_server` redevient une gate réelle :
  `test_migrations.py` s'exécute à chaque PR au lieu d'être skippé, et le round-trip
  `upgrade`/`downgrade` est exercé en continu plutôt qu'une fois à la main.
- Aucune extension de la surface d'attaque du runner partagé **au titre de l'exposition
  Docker** (la surface introduite par §3.3 est traitée en R-SEC-3) — l'invariant « pas de démon
  Docker dans le runner » passe d'un accident de configuration à une règle écrite et
  vérifiable par grep.
- Le pattern est déjà éprouvé : le coût de première mise en œuvre est une copie adaptée
  d'un step existant, pas une conception.
- L'épinglage PG 16 aligne la CI sur la prod pour tous les repos Python du fleet **et
  ferme une dérive active non détectée jusqu'ici** : la seule suite e2e DB du parc (Orion)
  valide aujourd'hui du PG 12 pour une prod PG 16. Le bénéfice n'est donc pas seulement
  prospectif pour Blue — il corrige un mensonge de gate déjà en place.
- Le versionnement du substrat rend enfin diffable et reviewable une configuration qui
  gouverne 18 CI.
- L'isolation par job est gratuite : le runner est éphémère, le cluster meurt avec lui.

**Négatives / coûts**

- Duplication du step de démarrage PG dans chaque `ci.yml` concerné (pas de
  `workflow_call` ni d'action composite partagée dans le fleet Zab ; une action composite
  cross-repo exigerait un token de checkout, c'est-à-dire une surface de plus).
  Duplication assumée en v1, détectable par grep — même arbitrage qu'ADR 004 §4.
- Le rebuild de l'image runner est une opération VPS manuelle avec indisponibilité du
  pool ; à faire hors fenêtre de charge CI.
- La mémoire du cluster PG s'impute au plafond du runner ; certains repos demanderont un
  override.
- Un repo qui aurait besoin d'une autre version majeure de PG n'est pas servi par ce canon
  (voie de sortie : option C).

## 5. Risks

- **R-SEC-1 (traité — verdict Bastion)** — *Exposition du démon Docker.* Écartée par
  §3.6 B, sur verdict NO-GO ferme de Bastion. Résiduel : nul par construction, aucune
  permission n'étant accordée.
- **R-SEC-2** — *Credentials de la base CI.* Ils restent littéraux dans le YAML, locaux au
  conteneur, sans valeur hors du job. Interdiction explicite d'en faire des GitHub Secrets
  ou de les faire pointer vers une base non éphémère. Un `secret-scan` qui alerterait
  dessus se traite par allowlist ligne-à-ligne, jamais en désarmant le job
  (ADR 004 §3.8.1).
- **R-1** — *Drift du substrat.* Le rebuild d'image ou un `docker compose` upstream de
  l'orchestrateur peut réinstaller une image sans PG, ou repasser `RUNNER_IMAGE` sur
  l'image vanilla — c'est exactement l'incident du 2026-06-28. Mitigation : §3.3
  (versionnement) + la branche skip-apt qui dégrade au lieu de casser (le job réinstalle
  PG, plus lent, toujours vert) + l'échec explicite sur version majeure ≠ 16.
- **R-2** — *Contention du pool.* Un job PG est plus long qu'un job lint ; sur un pool à
  ~6 slots, il allonge la file. Mitigation : n'ouvrir aucun job supplémentaire dédié au démarrage de la base (§3.4), adoption
  opt-in et non big-bang (§3.5). La piste de fusion des gates sub-30s (lockfile /
  CODEOWNERS / secret-scan) reste ouverte hors scope.
- **R-3** — *OOM sur `RUNNER_MEM=1g`.* Mitigation : override par repo sur preuve, jamais
  préventivement.
- **R-4 (résiduel accepté)** — *Pas d'isolation entre le cluster PG et le job.* Le test
  tourne en superuser sur un cluster local. Acceptable : le conteneur est éphémère et
  détruit en fin de job ; c'est déjà le régime d'Orion.
- **R-5** — *Absence de canon Zab découvrable.* Tant que RC 13 n'est pas satisfait, un
  agent travaillant sur un repo Zab n'a aucun texte lui interdisant `services:`/`docker
  run` là où il le cherche — le `CLAUDE.md` du repo et ses `@imports`. C'est ce qui a
  coûté la PR #222. Mitigation : RC 13. L'ADR 004 de l'étage 0 n'est pas en cause
  (scopé G2) et n'a pas à être modifié.
- **R-SEC-3 (→ Bastion, clearance requise avant bascule)** — *Le repo de build de l'image
  runner devient un point de contrôle sur les 18 CI, adjacent à la prod.* Traité par le
  régime d'accès de §3.3 (privé, CODEOWNERS, branch protection, rebuild manuel Keeper).
  Le threat model rendu par Bastion est scopé à l'exposition du socket et **ne couvre pas**
  cette surface : une clearance dédiée est exigée avant que le repo devienne la source de
  vérité du build. *Comparatif honnête* : le statu quo hand-managed porte la même surface
  sans revue ni historique — le risque n'est pas créé, il est déplacé vers un endroit
  contrôlable.
- **R-6 (résolu — n'est plus une hypothèse)** — *Version majeure du pool.* Établie :
  **PG 12** (run `31309561765`). Le risque résiduel se déplace sur la **migration** : la
  cohabitation PG 12 / PG 16 dans l'image (§3.2) est le mode d'échec silencieux à
  surveiller, et le rebuild d'image est l'opération à risque, pas le relevé.
- **R-7** — *Rupture de la CI d'Orion par le changement de version.* Passer son e2e de
  PG 12 à PG 16 corrige une dérive mais **change le moteur sous une suite qui n'a jamais
  tourné dessus** : un comportement implicitement dépendant de PG 12 se révélerait rouge
  au premier run. C'est un bénéfice (la CI dirait enfin la vérité sur la prod), pas une
  régression, mais il doit être anticipé plutôt que subi — d'où un critère dédié (§6.6) et
  un ordre d'exécution qui met Orion en premier canari, avant Blue.
- **R-8 (risque ouvert, hors périmètre de cet ADR — porteur, org-owner)** — *Réglage
  `allows_public_repositories` du runner group inconnu.* Bastion n'a pas pu le vérifier
  (403 : son App n'a pas `administration:read` au niveau org). Le garde fork de §3.1 et
  RC 5 en sont la mitigation côté repo, et c'est le bon geste — mais la clause de RC 5 qui
  autorise à justifier un job sans garde par « repo privé, pas de fork possible » **ne tient
  que si ce réglage est connu**. Tant qu'il ne l'est pas, cette justification n'est pas
  recevable et le garde fork s'applique sans exception. Vérification à faire par le porteur.

**Rollback** : la modification `ci.yml` d'un repo se révoque par revert de la PR (aucun
état persistant). Le rebuild de l'image runner se révoque par le chemin déjà documenté
(`cp docker-compose.yml.bak docker-compose.yml` + relance avec `APP_PRIVATE_KEY`), et le
`RUNNER_IMAGE` de l'orchestrateur par restauration du `.env.bak` daté +
`docker restart zab-runner-orchestrator` — une ligne, réversible. Le retour à l'image
pré-PGDG est un rebuild depuis le `Dockerfile` versionné au commit précédent (§3.3), ce
qui est précisément le bénéfice du versionnement.

## 6. Resolution criteria

Chaque critère nomme son **porteur** et sa **preuve**. Un critère sans commande ni
artefact vérifiable n'en est pas un.

1. **PG 16 servi par l'image** — *porteur : Keeper.* Preuve :
   `docker run --rm --entrypoint pg_lsclusters zab-org-runner:pg -h` renvoie une **unique**
   ligne, de version majeure `16` (l'unicité vaut preuve que le cluster 12 hérité a bien
   été retiré, §3.2). Sortie collée dans l'issue d'implémentation.
2. **Échec explicite sur mauvaise version** — *porteur : Keeper.* Preuve : le job porte
   l'assertion de version, et le rouge délibéré est produit **par cette méthode**, sur une
   branche jetable : forcer temporairement l'assertion à exiger `17`, pousser, constater le
   job rouge avec le message attendu, supprimer la branche. Lien du run rouge dans l'issue.
   Aucune modification de l'image n'est requise pour ce test.
3. **Aucun `services:` dans les workflows Zab** — *porteur : Keeper.* Preuve : sortie de
   `grep -rn "^[[:space:]]*services:" .github/workflows/` vide, exécutée sur la **liste
   nommée** des repos servis par le pool (celle de `RUNNER_SERVED_REPOS`, reproduite dans
   l'issue), pas sur un « chaque repo » déclaratif.
4. **Aucun accès Docker depuis un job** — *porteur : Keeper.* Preuve : même méthode et même
   liste, motif `docker\.sock|/var/run/docker`, sortie vide ; **plus** l'inspection du
   compose runner versionné montrant qu'aucun montage de socket n'est déclaré vers les
   conteneurs runner.
5. **Garde fork présent** — *porteur : Keeper.* Preuve : sur la même liste de repos, tout
   job `runs-on: [self-hosted, …]` porte la condition
   `head.repo.full_name == github.repository` ; les manquants sont listés nommément puis
   corrigés ou justifiés (repo privé sans fork possible). Recoupe R4bis de Bastion.
6. **Orion vert sur PG 16** — *porteur : Keeper.* Preuve : le job `e2e (Postgres)` d'Orion
   est vert **après** bascule de l'image, sur un run identifié. Orion est le **premier
   canari** : il porte la seule suite e2e DB existante et c'est lui qui subit le changement
   de moteur (R-7). Blue ne bascule qu'après.
7. **CI Blue verte avec Postgres réel** — *porteur : Keeper.* Preuve : un run de PR sur
   ZabLaboratory/Blue exécute `tests/blue_server` **sans skip** — `test_migrations.py`
   apparaît en `passed` et non `skipped` dans la sortie pytest du run — round-trip
   `alembic upgrade head` / `downgrade base` inclus.
8. **Idempotence prouvée** — *porteur : Keeper.* Preuve : le même job vert sur deux runs
   consécutifs portés par des runners éphémères **distincts** (identifiants relevés dans
   les logs), et vert sur un runner JIT comme sur un runner statique.
9. **Pas de régression de pool** — *porteur : Keeper.* Preuve : après rebuild, un job d'un
   repo non-DB nommé tourne vert sur le pool basculé, et **aucun job ne reste `queued` plus
   de 10 minutes** sur une fenêtre d'observation de **2 heures** suivant le rebuild
   (bornes issues du régime nominal du pool : ≈6 slots, jobs majoritairement < 5 min).
10. **Substrat versionné et traçable jusqu'à l'image en service** — *porteur : Keeper.*
    Preuve : le `Dockerfile` et le `.env.example` existent dans le repo d'accueil, **et**
    le lien image↔commit est matérialisé — le `Dockerfile` inscrit le SHA du commit de
    build en label OCI (`org.opencontainers.image.revision`), lisible par
    `docker image inspect zab-org-runner:pg` sur ce label, dont la sortie correspond à un
    commit existant du repo. Sans ce label, « versionné » resterait invérifiable.
11. **Régime d'accès du repo d'accueil en place** — *porteur : Eleven + Keeper ; clearance
    Bastion.* Preuve : repo privé, `CODEOWNERS` couvrant racine et `Dockerfile`, branch
    protection active sur `main` (configuration relevée via l'API GitHub, sortie jointe),
    et **clearance Bastion écrite** sur ce régime (R-SEC-3) avant que le repo devienne la
    source de vérité du build.
12. **Verdict Bastion référencé nommément** — *porteur : Vigil à l'acceptation.* Le NO-GO
    sur l'exposition de `/var/run/docker.sock` (directe ou proxifiée) est cité dans cet ADR
    avec sa provenance exacte : **Bastion, work unit
    `ZAB-CI-POSTGRES-NATIVE-THREATMODEL-20260811`, `AGENT_REPORT` du 2026-08-11 sur
    ZabLaboratory/Blue#224**, motifs R1→R2 (chaîne jusqu'à la compromission de plateforme)
    et R6 (proxy GET-only de `docs/rules/security.md:28-29` structurellement inapplicable,
    `services: postgres` exigeant `POST /containers/create` dont le corps porte les
    bind-mounts). Une réouverture exige un amendement citant cette même référence.
13. **Canon Zab découvrable** — *porteur : Scribe.* Assertion **positive** et falsifiable :
    `agents/_shared/conventions.md` (étage 1, carte transverse Zab) porte une entrée
    « Postgres en CI » énonçant le canon natif, l'interdit `services:` et le garde fork, et
    pointant explicitement vers `Orion/docs/adr/018-…` et vers l'implémentation de
    référence `Orion/.github/workflows/ci.yml:64-137`. Preuve : le texte existe et un
    `grep` de `018-ci-postgres` sous `agents/_shared/` renvoie au moins une occurrence.
    *Désambiguïsation* : l'« ADR 004 » cité en §1 est
    `D:\Documents\docs\adr\004-cicd-skeleton-etage2.md` (**étage 0, scopé G2**). Il ne
    prescrit rien pour Zab et n'a donc **pas** à être corrigé : sa mention relève du
    contraste (ce qui vaut pour G2 ne vaut pas ici), pas d'un drift à réparer. Aucun autre
    « ADR 004 » n'est visé.
