# Runbook — Orion deploy-dir drift & deploy failure (`APP_PATH` BOM + Postgres secret mismatch)

- **Service** : Orion (ZabLaboratory) — VPS `vps-ovh` (`51.91.126.43`, user `ubuntu`)
- **Réseau** : `zab-internal` (backend), `caddy-public` (gateway). Gateway : `zabgate.cyell.dev/orion`.
- **Ports** : 4007 (api/health) + 4017, servis sur `zab-internal`.
- **Date incident traité** : 2026-06-07
- **Auteur** : Keeper (tier merge), chaîne `/fix` orchestrée par Eleven
- **PR de fix image** : #46 (`fix(deploy): harden Orion prod image — OCI labels + container healthcheck`), merge commit `b93e48a`
- **Statut final** : les **deux secrets cassés sont corrigés** (BOM retiré de `VPS_APP_PATH`,
  `ORION_PG_PASSWORD` resynchronisé) et le drift de chemin est résolu (le deploy cible désormais
  le vrai `/home/ubuntu/orion`). Mais le deploy reste ROUGE sur une **troisième cause distincte
  découverte à la remédiation** : un bug de passage d'argument dans le step `Install Solar bundles`
  de `ci.yml` (voir §2c). Prod **jamais interrompue** (ancien conteneur toujours up). Incident
  **PARTIELLEMENT résolu** ; le dossier BOM est **conservé** (point 4 non vert).

> **Noms réels des secrets** (confirmés par Bastion) : le secret de chemin est `VPS_APP_PATH`
> (pas `APP_PATH`), et le secret de password est `ORION_PG_PASSWORD` (pas `ORION_DATABASE_URL` ;
> ce dernier n'est pas un secret — il est construit par interpolation dans le `.env` distant à
> partir de `ORION_PG_PASSWORD`). Les anciennes mentions ci-dessous sont conservées pour l'historique.

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
>
> **Résolution** : le password legacy valide vit dans `/home/ubuntu/orion/.env` (32 octets,
> authentifie OK). Le secret `ORION_PG_PASSWORD` portait une autre valeur (43 octets) jamais
> appliquée au rôle SQL → `28P01`. Resync = recopier le legacy dans le secret (pipe direct
> VPS→`gh secret set`, jamais affiché).

### 2c. Bug de passage d'argument au step `Install Solar bundles` (cause découverte à la remédiation)

Une fois `VPS_APP_PATH` nettoyé, le deploy avance jusqu'au drift résolu (« Ensure target dir »
résout bien `/home/ubuntu/orion`, mtime du jour), puis **échoue plus loin**, au step
`Install Solar bundles` :

```
mkdir: cannot create directory ‘C:/Program’: Not a directory
```

Le step exécute `$SSH_CMD bash -s -- "$APP_PATH" "$SOLAR_VERSIONS" << 'INSTALL_SCRIPT'`. Le chemin
absolu passé en **argument positionnel** (`-- "$APP_PATH"`) est victime d'une **conversion de
chemin de type MSYS/Git-Bash** (`/home/...` → `C:/Program Files/Git/...`, tronqué au premier
espace → `C:/Program`), d'où le `mkdir` sur `C:/Program`. Reproductibilité prouvée : 3 runs
identiques échouent au même point ; le même heredoc lancé en **interpolation** (comme le step
« Ensure target dir » qui, lui, marche : `"mkdir -p '$APP_PATH'"`) crée correctement
`/home/ubuntu/orion/solar` (vérifié à la main sur le VPS).

> **Ce n'est ni un secret, ni de l'infra pure** : c'est un bug de **code de workflow** (`ci.yml`),
> introduit par la réécriture du téléchargement Solar dans PR #46. Le correctif (cesser de passer
> le chemin en argument positionnel ; l'interpoler dans le heredoc, comme « Ensure target dir »)
> est un **diff de code** → review Vigil requise, hors exception hotfix auto-mergeable de Keeper.

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

## 6. État après remédiation 2026-06-07 (sous clearance Bastion) & reste à faire

**Fait (sous clearance Bastion, exécuté par Keeper) :**

1. ✅ **`VPS_APP_PATH` nettoyé** : `gh secret set VPS_APP_PATH --body "/home/ubuntu/orion"`
   (chaîne ASCII littérale, octets `2f 68 6f 6d 65…6f 6e`, aucun BOM, aucun newline). Effet prouvé :
   « Ensure target dir » résout désormais `/home/ubuntu/orion` (mtime du jour) au lieu de l'arbre BOM.
2. ✅ **`ORION_PG_PASSWORD` resynchronisé** sur le password legacy valide, pipe direct
   VPS→`gh secret set --body -` (valeur jamais affichée ni écrite sur disque).

**Bloquant restant (hors périmètre auto-merge Keeper — code de workflow) :**

3. ⛔ **Corriger le step `Install Solar bundles` de `ci.yml`** (§2c) : ne plus passer `$APP_PATH`
   en argument positionnel `bash -s -- "$APP_PATH"` (mangling MSYS → `C:/Program`), mais
   l'interpoler dans le heredoc (forme du step « Ensure target dir » qui fonctionne). **Diff de
   code → branche `keeper/<slug>` + review Vigil.** Tant que ce step échoue, le deploy reste rouge.

**Après le fix #3 (à enchaîner) :**

4. **Re-déployer** (re-run du job `Deploy`). Cible attendue : `/home/ubuntu/orion` passe en Go,
   migration goose OK, conteneur recréé `healthy`, image distroless, labels OCI `revision`/`source`
   non-`unknown`, smoke `health/ready=200`, operator `401`.
5. **Purger le dossier BOM** seulement après deploy vert ET re-scan de références = 0
   (compose/scripts/cron/systemd/Caddy/deploy). Garder `orion.go-bom.bak-$ts` un temps.
6. **Révocation du PAT longue-vie** par le porteur (action humaine, hors Keeper) — après deploy vert.

## 7. Prévention

- **`VPS_APP_PATH` / chemins de deploy (risque R3 Bastion)** : durcir le job `preflight-deploy` —
  aujourd'hui il ne teste que la **présence** (`-z`) des secrets, pas leur forme. Ajouter une
  **assertion préflight** qui **rejette** un secret de chemin contenant un BOM (U+FEFF), un
  whitespace, ou non absolu (`case "$P" in /*) ;; *) exit 1 ;; esac` + test BOM via `od`/`printf '%q'`).
  C'est précisément ce qui aurait bloqué l'incident à la source.
- **Passage de chemins via SSH dans les workflows** : ne jamais passer un chemin absolu en
  **argument positionnel** à un `bash -s --` distant (mangling MSYS → `C:/Program`). Interpoler
  dans le corps du heredoc (forme « Ensure target dir »). Cf. §2c.
- **Diagnostic** : l'IMAGE qui tourne fait foi (`docker inspect`), pas le contenu du répertoire.
- **Postgres** : ne jamais changer un secret de password sans rotation coordonnée du rôle SQL ;
  un `POSTGRES_PASSWORD` ne s'applique qu'à l'init du volume.
- **Image durcie** (objet de #46) : labels OCI `revision`/`source` obligatoires (build-args
  `ORION_GIT_REVISION`/`ORION_GIT_SOURCE`) + healthcheck conteneur — pour rendre l'image traçable
  et détecter ce genre de drift au `docker inspect`.
- **Download Solar public sans PAT** : déjà en place (curl public), à conserver.
- **Issue de durcissement** : vérification checksum des bundles Solar au téléchargement.
