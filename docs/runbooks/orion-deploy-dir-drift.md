# Runbook — Orion deploy : drift de répertoire + deux secrets corrompus par le shell de set

- **Service** : Orion (ZabLaboratory) — VPS `vps-ovh` (`51.91.126.43`, user `ubuntu`)
- **Réseau** : `zab-internal` (backend), `caddy-public` (gateway). Gateway : `zabgate.cyell.dev/orion`.
- **Ports** : 4007 (api/health) + 4017 (internal), servis sur `zab-internal` (pas de port hôte).
- **Date incident traité** : 2026-06-07
- **Auteur** : Keeper (tier merge), chaîne `/fix` orchestrée par Eleven
- **Workflow concerné** : le job `deploy` vit dans **`.github/workflows/ci.yml`** (il n'y a pas
  de `deploy.yml` séparé ; `ci.yml` exécute build/test/lint + le pipeline de deploy push-to-main).
- **PRs** : #46 (durcissement image — OCI labels + healthcheck, merge `b93e48a`), #48
  (`fix(deploy): interpolate remote paths instead of bash -s positional args`, merge dans `main` `20f1318`).
- **Statut final (2026-06-07)** : **PARTIELLEMENT résolu.** Le drift de répertoire est **corrigé**
  (`/home/ubuntu/orion` contient désormais le Go, plus de Python ; aucun nouvel arbre fantôme).
  Le deploy reste **ROUGE** sur un **second secret corrompu** non encore re-setté :
  `ORION_PG_PASSWORD` → migration goose `28P01`. Prod **jamais interrompue** (conteneur Go up depuis
  5 semaines). Arbres fantômes **conservés** (point 4 non vert).

---

## 0. Cause racine en une phrase

Deux secrets GitHub Actions ont été **corrompus par le shell qui les a settés** (Git-Bash/Windows) :
`VPS_APP_PATH` (BOM puis conversion de chemin MSYS) et `ORION_PG_PASSWORD` (valeur réduite à un seul
caractère `-`). Le **runner Linux** (`ubuntu-latest`) et le **conteneur Go** étaient sains tout du
long. Le `VPS_APP_PATH` a été re-setté **proprement via PowerShell** (zéro conversion MSYS) ; il
reste à faire de même pour `ORION_PG_PASSWORD`.

## 1. Symptômes

- Runs `Deploy` (job dans `ci.yml`) en échec répétés sur `main`.
- Le répertoire de déploiement `/home/ubuntu/orion` contenait un Orion **Python périmé**
  (`pyproject.toml`, `alembic/`, `uv.lock`, `src/`, Dockerfile non-Go), alors que le conteneur
  `orion` en prod tournait — et tourne toujours — une image **Go distroless**
  (`orion-orion` sha `032895d81079`, up depuis le 2026-05-02, ~5 semaines).
- Des **arbres de répertoires fantômes** existaient sous `/home/ubuntu/` :
  - `/home/ubuntu/<U+FEFF>/home/ubuntu/orion/` (top-level nommé du seul octet-BOM `EF BB BF`) ;
  - `/home/ubuntu/C:/Program Files/Git/home/ubuntu/...` (chemin Windows MSYS matérialisé en arbo).

## 2. Cause racine (prouvée par commandes, pas déduite)

Deux causes distinctes, **toutes deux des secrets corrompus à l'écriture** — pas de l'applicatif,
pas du runner.

### 2a. `VPS_APP_PATH` corrompu par le shell de set (BOM puis MSYS) → drift + arbres fantômes

Le secret `VPS_APP_PATH` aurait dû valoir `/home/ubuntu/orion`. Il a été pollué **deux fois** par
le shell Windows qui l'a écrit :

1. **BOM (U+FEFF)** en tête → le deploy résolvait `/home/ubuntu/<U+FEFF>/home/ubuntu/orion`
   (l'arbre fantôme « BOM »).
2. **Conversion de chemin MSYS/Git-Bash** : `/home/ubuntu/orion` réécrit par MSYS en
   `C:/Program Files/Git/home/ubuntu/orion`, d'où l'arbre fantôme `/home/ubuntu/C:/Program Files/Git/...`.

Le « vrai » `/home/ubuntu/orion` n'étant jamais ciblé, il a **divergé** : resté figé sur l'ancien
Python pendant que le conteneur tournait du Go. C'est le **drift de répertoire**.

Preuve au niveau run (avant fix), step `Write remote .env` :

```
cat: Files/Git/home/***/orion/.env: No such file or directory
```

Le `$APP_PATH` **non quoté** dans ce step a subi un word-split sur l'espace de `C:/Program Files/...`
→ `cat > C:/Program` puis `Files/Git/...` traités comme args séparés.

> **L'IMAGE fait foi, pas les fichiers du répertoire.** Le diagnostic correct part de
> `docker inspect orion --format '{{.Image}}'` (ce qui tourne réellement), pas du contenu de
> `/home/ubuntu/orion` (qui peut être un répertoire mort divergé).

### 2b. `ORION_PG_PASSWORD` corrompu (réduit à `-`) → migration goose `28P01`

Une fois `VPS_APP_PATH` re-setté proprement, le re-run a avancé **jusqu'à `Build, migrate, restart`**,
où goose échoue :

```
goose run: failed to connect to `user=orion database=orion`:
172.26.0.4:5432 (orion-postgres): failed SASL auth:
FATAL: password authentication failed for user "orion" (SQLSTATE 28P01)
```

Diagnostic VPS (read-only, valeurs jamais affichées) :

- Le `.env` écrit par le deploy porte `ORION_PG_PASSWORD=-` — **un seul caractère** (`len=1`,
  octets `2d`). `ORION_DATABASE_URL` (construit par interpolation depuis ce secret) porte donc aussi
  un password de longueur 1. → c'est le **secret `ORION_PG_PASSWORD` lui-même qui est corrompu**,
  pas une « désync URL ».
- Le rôle SQL `orion` dans le volume `orion_pg_data` a son **vrai password = 32 octets** :
  il s'authentifie **OK sur TCP** avec la valeur du backup legacy `/home/ubuntu/orion.bak-$ts/.env`.
- La valeur 43 octets du backup `orion.go-bom.bak-$ts/.env` **ne s'authentifie PAS** (43 ≠ rôle).
- La valeur `-` (secret courant) **ne s'authentifie PAS** sur TCP → `28P01`.

> Un `POSTGRES_PASSWORD` n'est appliqué qu'à la **première** initialisation du volume. Le rôle `orion`
> garde donc son password d'origine (32 octets) quels que soient les restarts. Le mismatch est
> purement côté **secret corrompu**.
>
> Le `psql ... -c "select 1"` lancé en local via le socket Unix **réussit** même avec un mauvais
> password : `pg_hba.conf` utilise `trust`/`peer` sur le socket. Seules les connexions **TCP**
> (goose, depuis le réseau `zab-internal`) exigent le password — d'où la nécessité de tester
> l'auth **sur TCP**, pas via le socket, pour ne pas masquer le bug.

## 3. Arbres fantômes (le piège)

- **Arbre BOM** : `/home/ubuntu/<U+FEFF>/home/ubuntu/orion/` — le top-level a pour seul nom l'octet
  BOM (`EF BB BF`). Localisation fiable (l'expansion shell d'un BOM littéral échoue) :

  ```bash
  bomtop=$(find /home/ubuntu -maxdepth 1 -name "*$(printf '\357\273\277')*" -type d -print -quit)
  ```

- **Arbre MSYS** : `/home/ubuntu/C:/Program Files/Git/home/ubuntu/...` (le `C:` littéral est un nom
  de répertoire valide sous Linux).
- Faux positif connu : le BOM en tête de `g2.caddyfile` (projet G2) est **sans rapport** avec Orion.

## 4. Remédiation (chronologie 2026-06-07)

Ordre de sécurité : **backups AVANT toute action**, purge des arbres fantômes **non exécutée**
(critères de résolution non verts).

1. **Backups / rollback** (`ts=20260607-015218`, vérifiés présents) :
   - `orion-orion:rollback-$ts` = image Go `032895d81079` (identique au conteneur courant) ;
   - `/home/ubuntu/orion.bak-$ts` (arbre Python legacy, porte le `.env` au **vrai** password 32 o) ;
   - `/home/ubuntu/orion.compose.bak-$ts` ;
   - `/home/ubuntu/orion.go-bom.bak-$ts` (copie de sûreté de l'arbre BOM ; son `.env` = 43 o, NON valide).
2. **Re-set propre de `VPS_APP_PATH` par Eleven via PowerShell** (zéro conversion MSYS, zéro BOM) —
   `repo-scope`, updated `2026-06-07T02:54:37Z`. **Keeper ne lance jamais `gh secret set`** (son
   Bash est Git-Bash/Windows → re-mangle le chemin). Effet **prouvé** au re-run :
   - step « Ensure target dir exists » résout `/home/ubuntu/orion` (propre, ni BOM ni `C:/Program`) ;
   - `Sync code to VPS`, `Install Solar bundles`, `Write remote .env` **passent** ;
   - `/home/ubuntu/orion` contient désormais le **Go** (`go.mod`, `cmd/orion/main.go`, `internal/`,
     `migrations/`, `solar/`, mtime du jour `02:57`) ; **plus de** `pyproject.toml`/`uv.lock`/`alembic`
     (nettoyés par `rsync --delete`) → **drift résolu** ;
   - **aucun nouvel arbre fantôme** : `ls /home/ubuntu` ne montre toujours que les deux arbres
     préexistants (`C:`, BOM).
3. **Deploy ROUGE** au step `Build, migrate, restart` → goose `28P01` (§2b), car
   `ORION_PG_PASSWORD` est encore corrompu (`-`).
4. **Pas de rollback applicatif** : le deploy a planté **avant** `up -d --force-recreate orion` ;
   le conteneur et le volume n'ont **pas** été touchés. Prod intacte.
5. **Arbres fantômes NON purgés** (point 4 du protocole non vert).

## 5. Bloquant restant & marche à suivre

### Bloquant (hors périmètre Keeper — secret + surface sensible → Eleven + clearance Bastion)

⛔ **Re-setter `ORION_PG_PASSWORD` proprement via PowerShell** (comme `VPS_APP_PATH`), avec la
**bonne valeur** = le password du rôle SQL `orion` déjà figé dans le volume = **les 32 octets** du
backup `/home/ubuntu/orion.bak-$ts/.env` (vérifié : s'authentifie sur TCP ; url-safe, sans
espace/BOM/`:` → ne sera pas re-mangé par PowerShell).

- **Option A (retenue, zéro risque data)** : aligner le **secret** sur le password **existant** du
  volume (32 o). Aucune mutation DB, réversible.
- **Option B (rotation)** : choisir un nouveau password + `ALTER ROLE orion PASSWORD …` sur la DB
  live + re-set du secret. Rotation = surface sensible → **clearance Bastion + rollback documenté**.
  Non retenue ici (Option A suffit).

> Keeper **n'a ni lu ni affiché** la valeur du password : seuls la **longueur** et la
> **localisation** (`orion.bak-$ts/.env`) sont communiquées à Eleven, qui re-set via PowerShell.

### Après le re-set de `ORION_PG_PASSWORD` (à enchaîner)

1. **Re-run du job `Deploy`** (re-run failed du dernier run, ou push trivial sur `main`).
2. **Vérifications de résolution** (toutes via la prod) : goose applique (plus de `28P01`),
   `up -d --force-recreate orion` recrée le conteneur, conteneur `healthy`, image Go distroless,
   labels OCI `org.opencontainers.image.revision` / `.source` **non-`unknown`**,
   `curl https://zabgate.cyell.dev/orion/api/v1/health` = **200**, `/orion/api/v1/ready` = **200**,
   `POST …/scenes/x/push` sans token = **401**.
3. **Purge des arbres fantômes** (`/home/ubuntu/<BOM>/` et `/home/ubuntu/C:/`) **seulement** après
   deploy entièrement vert **ET** re-scan de références = 0 (compose/scripts/cron/systemd/Caddy).
   Conserver `orion.go-bom.bak-$ts` un temps.
4. **Révocation du PAT longue-vie** par le porteur (action humaine).

## 6. Rollback (référence)

Si une future tentative recrée le conteneur et casse la prod :

```bash
ts=20260607-015218
docker tag orion-orion:rollback-$ts orion-orion:latest
cp -a /home/ubuntu/orion.bak-$ts/. /home/ubuntu/orion/      # si le répertoire a été muté
cp /home/ubuntu/orion.compose.bak-$ts /home/ubuntu/orion/docker-compose.prod.yml
docker compose -f /home/ubuntu/orion/docker-compose.prod.yml up -d --force-recreate orion
# vérifier : docker inspect orion + curl https://zabgate.cyell.dev/orion/api/v1/health == 200
```

Backups conservés : `orion.bak-$ts`, `orion.compose.bak-$ts`, `orion.go-bom.bak-$ts`,
image `orion-orion:rollback-$ts`.

## 7. Prévention

- **Re-set des secrets depuis Windows** : **toujours** via **PowerShell** (`gh secret set` natif),
  **jamais** depuis Git-Bash/MSYS — qui injecte un BOM et/ou convertit les chemins POSIX
  (`/home/...` → `C:/Program Files/Git/...`) et peut réduire des valeurs spéciales. C'est la cause
  racine commune des **deux** corruptions de cet incident.
- **Durcissement `preflight-deploy` (C2, PR séparée à reviewer Vigil)** : le step ne teste
  aujourd'hui que la **présence** (`-z`) des secrets. Ajouter une **assertion de forme** qui
  **rejette** un `VPS_APP_PATH` contenant un BOM (U+FEFF), un whitespace, un `:` ou non absolu
  (`case "$P" in /*) ;; *) exit 1 ;; esac` + test BOM via `od`). Cela aurait bloqué l'incident à
  la source au lieu de le laisser muter le disque.
- **Quoter `"$APP_PATH"` dans les steps SSH de `ci.yml` (C2)** : le step `Write remote .env`
  utilise `$APP_PATH` **non quoté** → word-split sur l'espace d'un chemin mangé. Quoter partout
  (forme des steps déjà sûrs). Diff de code → review Vigil.
- **Diagnostic** : l'IMAGE qui tourne fait foi (`docker inspect`), pas le contenu du répertoire ;
  tester l'auth Postgres **sur TCP** (le socket Unix masque le mismatch via `trust`/`peer`).
- **Postgres** : ne jamais changer un secret de password sans rotation coordonnée du rôle SQL ;
  un `POSTGRES_PASSWORD` ne s'applique qu'à l'init du volume.
- **Image durcie** (objet de #46) : labels OCI `revision`/`source` + healthcheck conteneur — pour
  rendre l'image traçable et détecter le drift au `docker inspect`.
- **Download Solar public sans PAT** : déjà en place (curl public). Durcissement à suivre :
  vérification checksum des bundles Solar au téléchargement.
