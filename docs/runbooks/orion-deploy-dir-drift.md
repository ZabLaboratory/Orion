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
- **Statut final (2026-06-07)** : **RÉSOLU.** Les **deux** secrets corrompus ont été re-settés
  proprement via **PowerShell** par Eleven (`VPS_APP_PATH` updated `02:54:37Z`, `ORION_PG_PASSWORD`
  updated `03:04:35Z`). Le re-run du job `Deploy` sur `main` (run `27080585131`) est **VERT** de bout
  en bout : goose applique `0002_lsml_bundle.sql` (`migrated to version: 2`, plus de `28P01`),
  `up -d --force-recreate` recrée le conteneur. Prod vérifiée : conteneur `orion` `9d2b15458ab3`
  sur **nouvelle image** `9cd5f7c1…` (≠ rollback `032895d81079`), **healthy** (distroless, pas de
  `/bin/sh`), labels OCI `revision=20f1318…` / `source=github.com/ZabLaboratory/Orion`. Smoke gateway :
  `/orion/api/v1/health` = 200, `/orion/ready` = 200, `POST …/scenes/x/push` sans token = 401.
  `/home/ubuntu/orion` = Go, **aucun nouvel arbre fantôme**. Les **deux arbres fantômes ont été
  purgés** (ref-scan = 0) ; backups conservés.

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

Ordre de sécurité : **backups AVANT toute action**, purge des arbres fantômes **exécutée en dernier**,
après deploy entièrement vert + ref-scan = 0.

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
3. **Re-set propre de `ORION_PG_PASSWORD` par Eleven via PowerShell** — valeur = password legacy
   valide **32 octets** du rôle SQL `orion` figé dans le volume (Option A, zéro mutation DB).
   `repo-scope`, updated `2026-06-07T03:04:35Z`. Keeper n'a ni lu ni affiché la valeur.
4. **Re-run du job `Deploy`** sur `main` (run `27080585131`, `gh run rerun --failed`) → **VERT** :
   - `Build, migrate, restart` : `orion-postgres Healthy`, goose `OK 0002_lsml_bundle.sql` +
     `goose: successfully migrated database to version: 2` (plus de `28P01`) ;
   - `Container orion Recreated` (`up -d --force-recreate`), health interne `Orion healthy` ;
   - `Smoke test via gateway` : `Orion reachable via https://zabgate.cyell.dev/orion/*`.
5. **Vérification prod (indépendante de la CI)** : conteneur `orion` `9d2b15458ab3`, image
   `9cd5f7c1…` (≠ rollback `032895d81079`), `Up … (healthy)`, distroless (`/bin/sh` absent),
   labels OCI `revision=20f13183…` / `source=https://github.com/ZabLaboratory/Orion`.
   Gateway : health 200, ready 200, push-no-token 401. `/home/ubuntu/orion` = Go, aucun nouvel
   arbre fantôme.
6. **Purge des arbres fantômes** (point 4 du protocole, désormais vert) : ref-scan préalable = 0
   (aucun conteneur ne les référence en `working_dir`/`config_files`, aucun bind mount). Le tree
   BOM contenait une copie périmée du repo avec un `.env` orphelin (jamais lu) — sa suppression est
   un gain d'hygiène. `rm -rf /home/ubuntu/<U+FEFF>` + `rm -rf '/home/ubuntu/C:'` → confirmés
   `GONE`, non recréés au deploy suivant. **Backups conservés** : `orion.bak-$ts`,
   `orion.compose.bak-$ts`, `orion.go-bom.bak-$ts`, image `orion-orion:rollback-$ts`.
   Conteneur `orion` toujours `healthy` après purge.

## 5. Incident résolu — reliquats (hors périmètre / suivi)

L'incident est **clos** (deploy vert + prod vérifiée + arbres fantômes purgés). Restent des
éléments de suivi, dont aucun ne bloque la prod :

1. **Révocation du PAT longue-vie** par le porteur (action **humaine**) — un PAT a servi pendant
   l'incident ; à révoquer maintenant que le deploy est vert.
2. **C2 — durcissement `ci.yml` (PR séparée, review Vigil)** : (a) quoter `"$APP_PATH"` au step
   *Write remote .env* ; (b) garde preflight rejetant un `VPS_APP_PATH` à BOM / espace / `:` /
   non-absolu. Aurait bloqué l'incident à la source. **Diff de code → review Vigil**, pas un
   hotfix auto-mergeable.
3. **C4 — secret org** : rationaliser le scope des secrets de deploy (org vs repo) si décidé.
4. **R1/R2 — ADR** : tracer dans un ADR le risque résiduel (re-set de secrets depuis Windows,
   politique PowerShell-only) et la décision Option A (alignement secret↔volume, pas de rotation).
5. **Backup `orion.go-bom.bak-$ts`** : conservé volontairement (copie de sûreté de l'arbre BOM) ;
   à nettoyer plus tard une fois la confiance établie.

> Note Option A retenue à l'origine : la **bonne valeur** de `ORION_PG_PASSWORD` était le password
> du rôle SQL `orion` déjà figé dans le volume (32 octets, s'authentifie sur TCP) — pas une rotation.
> Aucune mutation DB, réversible. Keeper n'a **ni lu ni affiché** la valeur ; seuls longueur et
> localisation ont été communiqués à Eleven, qui a re-setté via PowerShell.

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
