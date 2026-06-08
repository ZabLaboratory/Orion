# Runbook — Rotation `ORION_PG_PASSWORD` (exposition transitoire hors-canal)

- **Date** : 2026-06-08
- **Opérateur** : Keeper
- **Sévérité** : moyenne (secret vu hors-canal, jamais sorti de l'infra)
- **Service** : Orion (`orion`) + sa Postgres (`orion-postgres`) sur `vps-ovh` (51.91.126.43)
- **Impact réel** : nul côté show — Orion idle/vide (`scenes_loaded: 0`), aucun show en cours.

## Pourquoi (déclencheur)

Lors du diagnostic VPS #48, un `sed` de rédaction a laissé `ORION_DATABASE_URL`
(qui embarque `ORION_PG_PASSWORD`) s'afficher **transitoirement** dans une session
SSH Keeper. La valeur **n'a pas quitté le VPS**, mais `security.md` impose la règle
« secret vu hors-canal ⇒ rotation immédiate » (asymétrie du coût : le coût d'une
rotation est trivial face au coût d'un credential compromis). Verdict Bastion : **rotation obligatoire**.

## Topologie concernée

- `ORION_PG_PASSWORD` vit à **trois** endroits qui doivent rester cohérents :
  1. Étage-1 : `D:\Documents\Zab\.env.orion` (`ORION_PG_PASSWORD` **et** embarqué dans `ORION_DATABASE_URL`).
  2. VPS : `/home/ubuntu/orion/.env` (mêmes deux variables ; consommé par `env_file` du compose).
  3. Le **rôle Postgres** `orion` dans le volume persistant `orion_pg_data`.
- `ORION_DATABASE_URL = postgres://orion:<pwd>@orion-postgres:5432/orion?sslmode=disable`
  (user `orion`, db `orion`, host `orion-postgres` sur le réseau `zab-internal`).
- **Piège** : l'image `postgres:16-alpine` n'applique `POSTGRES_PASSWORD` qu'à
  l'**init d'un data dir vide**. Le volume étant persistant, changer l'env **ne change
  pas** le mot de passe du rôle → il faut un `ALTER ROLE` explicite dans la DB vivante.

## Ordre d'exécution (anti-blocage)

Le rôle PG et l'env sont changés **avant** le restart d'Orion, pour qu'Orion reprenne
directement la nouvelle URL et ne tente jamais de se reconnecter avec un credential périmé.

1. **Générer** le nouveau secret sans l'afficher (pipe direct vers fichier 600) :
   ```
   umask 077
   python3 -c 'import secrets;print(secrets.token_urlsafe(48))' > /home/ubuntu/.orion-newpw
   chmod 600 /home/ubuntu/.orion-newpw
   ```
2. **ALTER ROLE** dans `orion-postgres`, password passé par **stdin de psql** (jamais sur
   argv, jamais `echo`), littéral quoté/échappé :
   ```
   NEWPW=$(cat /home/ubuntu/.orion-newpw)
   printf "ALTER ROLE orion WITH PASSWORD %s;" "$(printf '%s' "$NEWPW" | sed "s/'/''/g; s/^/'/; s/$/'/")" \
     | docker exec -i orion-postgres psql -U orion -d orion -v ON_ERROR_STOP=1 -q
   unset NEWPW
   ```
3. **Mettre à jour `/home/ubuntu/orion/.env`** (backup d'abord) : remplacer
   `ORION_PG_PASSWORD=` et le segment password de `ORION_DATABASE_URL=`
   (password **url-encodé** dans l'URL). Fait par script Python, sans afficher la valeur.
   Backup : `/home/ubuntu/orion/.env.bak-rotation-<ts>` (mode 600).
4. **Recréer** uniquement le conteneur `orion` (relit `env_file`) — `orion-postgres`
   n'est **pas** restarté (ALTER ROLE live ; POSTGRES_PASSWORD non relu sur volume existant) :
   ```
   cd /home/ubuntu/orion
   docker compose -f docker-compose.prod.yml up -d --no-deps --force-recreate orion
   ```
5. **Aligner l'étage-1** `D:\Documents\Zab\.env.orion` avec la même valeur (rapatriée
   du VPS en base64, jamais affichée). Cohérence vérifiée par **hash** des lignes
   sensibles (étage-1 == VPS), jamais par affichage de valeur.
6. **Détruire** le fichier temporaire : `shred -u /home/ubuntu/.orion-newpw`.

## Vérification (résultats obtenus)

- `orion` healthy en ~6s après recreate ; `orion-postgres` jamais interrompu (Up 23h).
- `GET http://orion-api:4007/api/v1/ready` (via curl jetable sur `zab-internal`) →
  **200** `{"database":"ok","scenes_loaded":0,"status":"ok"}` → connexion PG avec le
  **nouveau** password OK (le `/ready` fait un DB ping ; goose migre au boot, donc
  auth DB déjà exercée).
- `GET /api/v1/health` → 200. Logs Orion : `orion starting` → `public server listening`,
  **aucune** ligne `password authentication failed` / error / fatal / panic.
- Cohérence étage-1 ↔ VPS : `sha256` des lignes `ORION_PG_PASSWORD`+`ORION_DATABASE_URL`
  **identique** des deux côtés.
- Preuve de la barrière : depuis un conteneur tiers sur `zab-internal` (chemin réseau
  réel d'Orion), un **faux** pwd → `FATAL: password authentication failed for user "orion"`
  (la ligne `host all all all scram-sha-256` de `pg_hba.conf` gouverne ce chemin ;
  les `trust` ne concernent que socket local / loopback intra-conteneur).

## Rollback

Si Orion ne se reconnecte pas (auth failure dans les logs / `/ready` ≠ 200) :

1. Restaurer l'env VPS depuis le backup :
   ```
   cp /home/ubuntu/orion/.env.bak-rotation-<ts> /home/ubuntu/orion/.env
   ```
2. Restaurer l'**ancien** password du rôle PG. L'ancien `ORION_PG_PASSWORD` se lit dans
   ce backup (mode 600) ; rejouer un `ALTER ROLE orion WITH PASSWORD '<ancien>'` par stdin
   (même méthode qu'à l'étape 2, sans l'afficher).
3. `docker compose -f docker-compose.prod.yml up -d --no-deps --force-recreate orion`.
4. Restaurer l'étage-1 depuis `D:\Documents\Zab\.env.orion.bak-rotation-<ts>`.

> Rollback **non destructif** : aucun volume ni donnée touchés ; seul le credential et
> le restart du conteneur applicatif sont en jeu.

## Nettoyage post-incident (à faire une fois la rotation confirmée stable)

- Les backups `.env.bak-rotation-*` (VPS) et `.env.orion.bak-rotation-*` (étage-1)
  contiennent l'**ancien** pwd. Il est révoqué sur le chemin réseau, mais ces backups
  doivent être purgés (`shred -u`) une fois la stabilité confirmée — ne pas laisser
  un credential périmé traîner sur disque.

## Surface sensible — contrôle a posteriori

Rotation d'un **secret** : surface sensible au sens `git.md` Gate §3 / `security.md`.
Action exécutée en hotfix opéré (réversible, impact nul car Orion idle), à **auditer
par Bastion a posteriori** : la valeur n'a été ni affichée ni logguée à aucune étape
(génération par pipe, ALTER via stdin, édition par script, comparaison par hash) ;
la barrière SCRAM réseau est prouvée ; backups de rollback identifiés pour purge.
