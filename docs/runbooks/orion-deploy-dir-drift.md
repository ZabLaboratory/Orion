# Runbook — Orion deploy-dir drift & deploy failure (`APP_PATH` BOM + Postgres secret mismatch)

- **Service** : Orion (ZabLaboratory) — VPS `vps-ovh` (`51.91.126.43`, user `ubuntu`)
- **Réseau** : `zab-internal` (backend), `caddy-public` (gateway). Gateway : `zabgate.cyell.dev/orion`.
- **Ports** : 4007 (api/health) + 4017, servis sur `zab-internal`.
- **Date incident traité** : 2026-06-07
- **Auteur** : Keeper (tier merge), chaîne `/fix` orchestrée par Eleven
- **PR de fix image** : #46 (`fix(deploy): harden Orion prod image — OCI labels + container healthcheck`), merge commit `b93e48a`
- **Statut final** : deploy `Deploy` ROUGE (échec migration) → **rollback non nécessaire** (prod jamais interrompue) → **incident PARTIELLEMENT résolu** : image durcie buildée et prouvée, mais **non mise en prod** ; deux causes secrets restent à corriger hors périmètre Keeper.

---

## 1. Symptômes

- 8 runs `Deploy` consécutifs en échec sur `main` avant l'intervention.
- Le répertoire de déploiement attendu `/home/ubuntu/orion` contenait un Orion **Python périmé**
  (`pyproject.toml`, `alembic/`, `uv.lock`, Dockerfile non-Go), alors que le conteneur `orion`
  en prod tournait — et tourne toujours — une image **Go** (`orion-orion:latest` = sha `032895d81079`,
  up depuis le 2026-05-02).
- Une **seule copie Go sur disque** existait, dans un dossier parasite à BOM (voir §3).

## 2. Cause racine (prouvée par commandes, pas déduite)

Deux causes distinctes, toutes deux liées à des **secrets désynchronisés** (pas de l'applicatif) :

### 2a. `APP_PATH` corrompu par un BOM (U+FEFF) → drift du répertoire de déploiement

Le secret GitHub Actions `APP_PATH` du workflow `Deploy` contient un **BOM (U+FEFF)** en tête.
Le déploiement résout donc son chemin cible vers :

```
/home/ubuntu/<U+FEFF>/home/ubuntu/orion
```

au lieu de `/home/ubuntu/orion`. Conséquences vérifiées le 2026-06-07 :

- le seul `go.mod` synchronisé par le run était dans le **dossier BOM** (mtime `01:56`), pas dans `/home/ubuntu/orion` ;
- le step « Write remote .env » a écrit le `.env` dans le **dossier BOM** (mtime `01:57:02`) ;
- `/home/ubuntu/orion/.env` n'a **jamais** été touché (mtime resté `2026-05-02`).

Le `git clone` initial avec une URL/chemin préfixé d'un BOM a créé ce répertoire fantôme ;
chaque deploy rsync ensuite dedans. Le répertoire « officiel » `/home/ubuntu/orion` n'étant
jamais mis à jour, il a **divergé** (resté sur l'ancien Python), d'où le drift observé.

> **L'IMAGE fait foi, pas les fichiers du répertoire.** Le diagnostic correct part de
> `docker inspect orion --format '{{.Image}}'` (ce qui tourne réellement), pas du contenu
> de `/home/ubuntu/orion` (qui peut être un répertoire mort).

### 2b. `ORION_DATABASE_URL` (secret) désynchronisé du volume Postgres → migration goose KO

Le run déclenché par le merge #46 a planté au step `Build, migrate, restart`, à la migration :

```
goose run: failed to connect ... 172.26.0.4:5432 (orion-postgres):
failed SASL auth: FATAL: password authentication failed for user "orion" (SQLSTATE 28P01)
```

Le `.env` écrit par le deploy (depuis le secret GitHub `ORION_DATABASE_URL`) porte un
**password qui ne correspond pas** au mot de passe figé dans le volume Postgres de prod
à son initialisation. Preuve : le `.env` legacy de `/home/ubuntu/orion` (password inchangé
depuis mai) **authentifie correctement** sur le réseau (`net_auth_ok`), tandis que le secret
GitHub, lui, est rejeté en `28P01`.

> Un `POSTGRES_PASSWORD` n'est appliqué qu'à la **première** initialisation du volume.
> Changer le secret après coup ne réécrit pas le volume → mismatch silencieux jusqu'au
> prochain deploy qui s'authentifie par le réseau.

## 3. Le piège du dossier BOM

- Chemin : `/home/ubuntu/<U+FEFF>/home/ubuntu/orion/` (le top-level `/home/ubuntu/<U+FEFF>`
  a pour seul nom le caractère U+FEFF, octets `EF BB BF`).
- À l'instant de l'incident il contenait la **seule copie Go sur disque** (+ son `.env`, + `solar/`).
- **Localisation fiable** (l'expansion shell d'un BOM littéral échoue — `cannot stat`) :

  ```bash
  bomtop=$(find /home/ubuntu -maxdepth 1 -name "*$(printf '\357\273\277')*" -type d -print -quit)
  bomgo="$bomtop/home/ubuntu/orion"
  ```

- Faux positif connu : l'unique match « BOM » dans `g2.caddyfile` est un BOM en tête de
  fichier G2 **sans rapport** avec Orion — ne pas le confondre avec une référence au dossier.

## 4. Remédiation exécutée (2026-06-07)

Ordre de sécurité respecté : **backups AVANT merge**, action destructive (purge BOM) **non exécutée**
car critères de résolution non verts.

1. **Backups / rollback** (`ts=20260607-015218`) :
   - `docker tag orion-orion:latest orion-orion:rollback-$ts` (= image Go `032895d81079`, vérifiée) ;
   - `cp -a /home/ubuntu/orion /home/ubuntu/orion.bak-$ts` + `orion.compose.bak-$ts` ;
   - copie de sûreté du BOM (seule Go disque) → `/home/ubuntu/orion.go-bom.bak-$ts`.
2. **Merge** PR #46 (`gh pr merge 46 --merge --delete-branch`, hooks respectés). Commit `b93e48a`.
3. **Deploy** déclenché au merge : ROUGE au step `Build, migrate, restart` → migration goose `28P01` (§2b).
   Steps amont OK (rsync, Solar bundles, .env) **mais dans le dossier BOM** (§2a).
4. **Vérification** : la prod n'a **jamais été interrompue** — le deploy a planté **avant**
   `up -d --force-recreate orion`. Le conteneur `orion` tourne toujours l'image Go `032895d81079`.
   Smoke gateway post-incident : `health=200`, `ready=200`, operator sans token = `401`.
5. **Pas de rollback applicatif nécessaire** (le conteneur et le volume n'ont pas été modifiés).
   Seul ré-alignement de propreté : `latest` re-tagué sur l'image qui tourne (`032895`), et la
   nouvelle image durcie préservée sous `orion-orion:built-b93e48a-20260607` (non déployée).
6. **Dossier BOM NON purgé** : critères du point 4 non verts ET le BOM reste la copie Go de secours
   et le répertoire réellement ciblé par le deploy.

## 5. Rollback (référence)

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
images `orion-orion:rollback-$ts` et `orion-orion:built-b93e48a-20260607`.

## 6. Reste à faire (hors périmètre auto-merge Keeper — surface secrets)

Ces actions touchent des **secrets** → clearance Bastion requise, pas un hotfix auto-mergeable :

1. **Nettoyer le secret `APP_PATH`** dans GitHub Actions (`ZabLaboratory/Orion`) : retirer le
   BOM (U+FEFF) en tête → doit valoir exactement `/home/ubuntu/orion`.
2. **Resynchroniser `ORION_DATABASE_URL`** (secret GitHub) avec le password réel du volume
   Postgres de prod — OU rotation maîtrisée du password (changer le rôle Postgres `orion`
   ET le secret de concert), rollback documenté.
3. Après 1+2 : **re-déployer** (re-run du job `Deploy`). Cible attendue : `/home/ubuntu/orion`
   passe en Go, migration goose OK, conteneur recréé `healthy`, labels OCI `revision`/`source`
   non-`unknown`, smoke `health/ready=200`, operator `401`.
4. **Purger le dossier BOM** seulement après deploy vert ET re-scan de références = 0
   (compose/scripts/cron/systemd/Caddy/deploy). Garder `orion.go-bom.bak-$ts` un temps.
5. **Révocation du PAT longue-vie** par le porteur (action humaine, hors Keeper).

## 7. Prévention

- **`APP_PATH` / chemins de deploy** : valider l'absence de BOM/whitespace dans les secrets de
  chemin (preflight `printf '%q'` ou `od -c` sur le secret avant `cd`/`rsync`).
- **Diagnostic** : l'IMAGE qui tourne fait foi (`docker inspect`), pas le contenu du répertoire.
- **Postgres** : ne jamais changer un secret de password sans rotation coordonnée du rôle SQL ;
  un `POSTGRES_PASSWORD` ne s'applique qu'à l'init du volume.
- **Image durcie** (objet de #46) : labels OCI `revision`/`source` obligatoires (build-args
  `ORION_GIT_REVISION`/`ORION_GIT_SOURCE`) + healthcheck conteneur — pour rendre l'image traçable
  et détecter ce genre de drift au `docker inspect`.
- **Download Solar public sans PAT** : déjà en place (curl public), à conserver.
- **Issue de durcissement** : vérification checksum des bundles Solar au téléchargement.
