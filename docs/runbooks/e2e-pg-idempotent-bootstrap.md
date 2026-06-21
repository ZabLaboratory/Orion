# Runbook — Reset déterministe du job `e2e (Postgres)` sur runner self-hosted

> Runner : `vps-ovh` (self-hosted, org-scoped). Job : `e2e` dans
> `.github/workflows/ci.yml`. Fix mergé : PR #214 (branche de #209).

---

## Symptôme

Le job `e2e (Postgres)` échoue sur le step **Start PostgreSQL natively** avec :

```
ERROR:  role "orion" already exists
```

ou toute erreur du même style (`database "orion_e2e" already exists`, etc.),
suivie d'un exit non-zero qui fait aborter le psql sous `ON_ERROR_STOP=1`.

## Cause racine

Le runner `vps-ovh` est **persistant et réutilisé** entre les runs GitHub
Actions. Le cluster PostgreSQL installé nativement (via `apt`) n'est pas
détruit entre les runs : le rôle `orion` et la base `orion_e2e` créés lors
d'un run précédent existent encore au démarrage du run suivant. Les
instructions `CREATE ROLE` et `CREATE DATABASE` sans garde idempotente
échouent alors, et `ON_ERROR_STOP=1` fait sortir psql en erreur fatale.

Ce problème n'existe **pas** sur les runners éphémères GitHub-hosted (chaque
run part d'une image fraîche). Il est propre aux runners self-hosted réutilisés.

## Procédure de diagnostic

1. Vérifier que l'échec se produit sur le step **Start PostgreSQL natively**
   (et non un step ultérieur comme les migrations goose ou les tests Go).
2. Lire les premières lignes du log psql : si elles contiennent
   `role "orion" already exists` ou `database "orion_e2e" already exists`,
   c'est le scénario décrit ici.
3. Confirmer que le runner est bien `vps-ovh` (label visible dans
   l'en-tête du job dans l'interface GitHub Actions).
4. Si le cluster PG ne répond plus du tout (`pg_isready` timeout) :
   vérifier l'état du service sur le VPS —
   `pg_ctlcluster <version> main status` — et relancer manuellement si
   nécessaire avant de re-trigger le run.

## Fix en place (PR #214)

Le step de bootstrap a été rendu **idempotent** par la séquence suivante,
exécutée en une seule passe psql (`-v ON_ERROR_STOP=1`) à partir d'un
fichier SQL stagé (`/tmp/orion_e2e_bootstrap.sql`) :

```sql
-- 1. Terminer toutes les connexions actives sur la base (évite le DROP bloqué)
SELECT pg_terminate_backend(pid)
FROM pg_stat_activity
WHERE datname = 'orion_e2e' AND pid <> pg_backend_pid();

-- 2. Supprimer la base si elle existe (repart d'un état propre)
DROP DATABASE IF EXISTS orion_e2e;

-- 3. Rôle gardé : créer seulement s'il n'existe pas, sinon ALTER
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'orion') THEN
    CREATE ROLE orion LOGIN PASSWORD 'orion_e2e_ci' SUPERUSER;
  ELSE
    ALTER ROLE orion LOGIN PASSWORD 'orion_e2e_ci' SUPERUSER;
  END IF;
END
$$;

-- 4. Recréer une base fraîche
CREATE DATABASE orion_e2e OWNER orion;
```

Le SQL est écrit dans un fichier avant d'être passé à psql (`-f`), ce
qui évite d'imbriquer un heredoc dans une commande `su -c "..."` (le shell
n'expande pas correctement les heredocs dans ce contexte).

Le cluster est démarré (ou redémarré s'il tournait déjà) via :
```bash
pg_ctlcluster "$PGVER" main start || pg_ctlcluster "$PGVER" main restart
```

Cette séquence est **re-entrant** : elle peut être rejouée un nombre
arbitraire de fois sans échec, quelle que soit l'état résiduel du cluster.

## Rollback

Ce fix est purement dans le workflow CI (`.github/workflows/ci.yml`).
Pour revenir à l'état précédent : `git revert` du commit PR #214 sur main.
Aucune migration de données, aucun changement applicatif.

## Voir aussi

- CI : `.github/workflows/ci.yml` — step `Start PostgreSQL natively (no Docker on the runner)` (job `e2e`)
- Runbook isolation : `docs/runbooks/e2e-shared-db-schema-isolation.md`
- PR #214 (fix bootstrap idempotent), PR #209 (runtime opérateur, contexte du chantier)
