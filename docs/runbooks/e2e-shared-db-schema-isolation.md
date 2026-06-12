# Runbook — e2e `23505` on shared Postgres → schema isolation

**Date** : 2026-06-12 · **Auteur** : Keeper · **PR** : #145 (squash `08a1ff1`) ·
**Type** : hotfix infra/CI auto-mergé (3 conditions réunies, voir bas de page).

## Symptôme

`main` rouge. Job CI `e2e (Postgres)` en échec :

```
push_api_test.go:96: apply migration ../../../migrations/0001_init.sql:
  ERROR: duplicate key value violates unique constraint
  "pg_type_typname_nsp_index" (SQLSTATE 23505)
--- FAIL: TestE2E_PushAPI_FirstPush_CreatesRow
--- FAIL: TestE2E_PushAPI_RePush_Idempotent
```

Conséquence : `deploy` (qui a `needs: [… e2e …]`) **skippé** → les commits déjà
sur main (#142 db.* atomics, #144 variable.get) **non déployés**. Prod intacte
(`/orion/api/v1/health` = 200) mais figée sur l'ancien binaire.

## Cause racine

Le job `e2e (Postgres)` lance **tout** l'arbre `tests/e2e/...` contre **un seul**
Postgres partagé (service container). Il y a **deux** helpers `requireDB` :

- `tests/e2e/push_test.go` (package `e2e`)
- `tests/e2e/contract/helpers_test.go` (package `contract`)

Les deux **réappliquaient les migrations prod** sur la DB partagée et
n'absorbaient que `42P07` (duplicate_table) / `42701` (duplicate_column).

Or les migrations prod utilisent des `CREATE TABLE` **stricts** (sans
`IF NOT EXISTS` — voulu). En PostgreSQL, chaque `CREATE TABLE` crée aussi un
**type composite implicite** du même nom dans `pg_catalog.pg_type`. À la 2e
application (le package `contract` tourne *après* le package parent, qui a déjà
migré), la collision se produit d'abord sur l'insertion dans `pg_type` →
**`SQLSTATE 23505`** sur `pg_type_typname_nsp_index`, **avant** le code friendly
`42P07`. Ce `23505` n'était pas dans l'allowlist d'idempotence → fatal.

C'est une **flakiness structurelle** : DB e2e partagée entre packages + migrations
réappliquées. L'allowlist ne faisait que masquer le crosstalk.

## Fix (cause, pas symptôme)

Isolation par **schéma Postgres éphémère** par `requireDB` (option structurelle,
préférée à l'élargissement de l'allowlist) :

1. chaque `requireDB` provisionne son propre schéma `e2e_<uuid>` ;
2. il le bake dans le `search_path` de la connexion via
   `options=-c search_path=<schema>` dans le DSN (hérité par **toutes** les
   connexions du pool — un `SET` par-connexion ne survivrait pas au multiplexage
   pgxpool) ;
3. les migrations s'appliquent dans un namespace **garanti propre** (aucune
   tolérance d'idempotence nécessaire — allowlist supprimée des deux helpers) ;
4. le schéma est **DROP CASCADE au cleanup** (pas d'accumulation sur DB persistante).

Bug latent révélé au passage : le helper `contract` n'appliquait que `0001`+`0002`
et reposait sur le package parent pour seeder `scene_validations` (0003) /
`show_state` (0004) dans le `public` partagé. Avec l'isolation, la validation-gate
du push handler tombait sur `relation "scene_validations" does not exist`. Fix :
le helper `contract` applique désormais les **quatre** migrations.

Fichiers touchés (tests uniquement, zéro applicatif) :
`tests/e2e/push_test.go`, `tests/e2e/contract/helpers_test.go`.

## Preuve

- CI PR #145 : tous jobs verts dont `e2e (Postgres)`.
- `e2e (Postgres)` **re-run** sur la même infra DB → success de nouveau
  (repeatabilité ; chaque run exerce déjà 30+ `requireDB` sur le même Postgres).
- Post-merge main run `27423074952` : tout vert, **`Deploy` exécuté et success**
  (était skippé) — internal health OK, gateway smoke OK attempt 1/8, goose migrate OK.
- Direct prod : `GET https://zabgate.cyell.dev/orion/api/v1/health` →
  `{"service":"orion","status":"ok"}`. Orion redéployé avec #142 + #144.

## Rollback

Le fix est purement tests (`tests/e2e/**`) : `git revert 08a1ff1` + merge.
Aucun impact runtime/prod, aucune migration prod modifiée — le rollback est sûr
et ne touche pas la DB de prod. (Si l'on revertait, on retomberait sur le `23505`
sur DB partagée ; ne reverter que si une régression CI inattendue apparaît.)

## Suivi / angles morts signalés

- **Diag local impossible** : ni Docker CLI ni Postgres local sur l'hôte Windows
  de session. La preuve réelle est donc la CI (le bon environnement : Postgres
  partagé du job). Acceptable ici car la CI EST le banc de reproduction.
- **Deux `requireDB` divergents** subsistent (un par package), avec helpers
  dupliqués (`provisionSchema`, `dsnWithSearchPath`). Candidat à factorisation en
  un helper interne partagé `tests/e2e/internal/...` — non bloquant, à confier à
  Forge/Probe si on veut réduire la dette. Le helper `contract` a un build-tag et
  un préambule expliquant pourquoi il vit séparé (défaut Forge historique sur
  `BlueprintNode.OutputAt`, désormais réparé) ; une réunification mérite une vraie
  passe, pas un patch.

## Conditions hotfix auto-mergé (git.md Gate pt 2)

1. **Urgent** : main rouge bloque le deploy de toute la campagne. ✅
2. **Infra pure** : seulement des helpers de test e2e ; aucun applicatif, aucune
   surface sensible (auth/secrets/réseau/deps). ✅
3. **Documenté ensuite** : ce runbook + corps de PR #145. ✅

Pas de veto Bastion en jeu. Contrôle a posteriori Vigil/Bastion possible via #145.
