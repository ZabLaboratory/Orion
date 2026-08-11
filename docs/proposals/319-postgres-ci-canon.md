# Proposition — canon "Postgres en CI" dans `Zab/agents/_shared/conventions.md`

> Issue `ZabLaboratory/Orion#319` (B6, ADR 018 RC 13). `conventions.md` vit à l'étage 1
> (`D:\Documents\Zab\agents\_shared\conventions.md`), hors git : Scribe propose ce diff
> littéral, Eleven l'applique (Write direct, sauvegarde datée du fichier avant écriture).
> Source : ADR 018 §3 (Decision), §3.1 (Forme canonique du job), révision
> `sha256:2562685ce5459ff28505028f2eadaf96618363484108ff3b8a8a88d6ccf04e3a`, merge
> `af78d5d9d0bdf8650c84cba9fc45849448106522`.

## Fichier cible

`D:\Documents\Zab\agents\_shared\conventions.md`, section `## Règles transverses`,
sous-section `### Tests` — insérer un nouveau bloc **après** le paragraphe existant sur
les DB (« Pas de mock pour les DB en Go/Python… ») et **avant** `### Lockfile`.

## Diff (avant → après)

### Avant

```markdown
### Tests

Les tests sont **exécutés, pas seulement affirmés** :

- Python : `pytest` + `pytest-cov` — floor 70 % overall, 90 % sur les
  chemins critiques (auth, crypto, proxy). La CI n'est pas verte sans tests verts.
- Go : `go test ./...` + `go test -tags e2e ./...`. Stdlib `testing`.
- TypeScript : `vitest` (unit) + optionnellement Playwright (E2E).
- Pas de mock pour les DB en Go/Python — les tests utilisent une vraie DB
  (Postgres en CI) ou SQLite (ZabAuth Python uniquement pour les tests unitaires).
- Tests marqués `@pytest.mark.live` (Quasar) skippés en CI par défaut ;
  exécution manuelle avec les vraies credentials Twitch.

### Lockfile
```

### Après

```markdown
### Tests

Les tests sont **exécutés, pas seulement affirmés** :

- Python : `pytest` + `pytest-cov` — floor 70 % overall, 90 % sur les
  chemins critiques (auth, crypto, proxy). La CI n'est pas verte sans tests verts.
- Go : `go test ./...` + `go test -tags e2e ./...`. Stdlib `testing`.
- TypeScript : `vitest` (unit) + optionnellement Playwright (E2E).
- Pas de mock pour les DB en Go/Python — les tests utilisent une vraie DB
  (Postgres en CI) ou SQLite (ZabAuth Python uniquement pour les tests unitaires).
- Tests marqués `@pytest.mark.live` (Quasar) skippés en CI par défaut ;
  exécution manuelle avec les vraies credentials Twitch.

**Postgres en CI (canon, ADR 018)** — le substrat runner self-hosted Zab
(`vps-ovh`, pool partagé `zab-org-runner:pg`) n'expose aucun démon Docker aux
conteneurs runner. Toute CI ayant besoin d'une base de données suit ce canon :

- **PostgreSQL natif dans le conteneur runner** (`pg_ctlcluster`, idempotent :
  skip-apt si déjà provisionné, démarrage/bootstrap rejouables). Pas d'ORM
  requis pour ce choix — c'est un choix de substrat CI, indépendant du client
  DB applicatif.
- **Interdit** : le bloc `services:` de GitHub Actions (exige un démon Docker
  pour l'init container — absent sur ce substrat, cause racine de l'échec
  Blue PR #222).
- **Interdit** : tout accès au démon Docker de l'hôte depuis un conteneur
  runner, direct ou proxifié (pas de montage `/var/run/docker.sock`,
  pas de socket-proxy).
- **Garde fork obligatoire** sur tout job tournant sur ce substrat (pas
  seulement les jobs DB) :
  `if: github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository`
  — sans lui, une PR ouverte depuis un fork exécute du code arbitraire sur un
  runner du pool partagé.
- Credentials de la base CI en `env:` du job, jamais en GitHub Secret (locaux
  au conteneur, éphémères, sans valeur hors du job).

  Référence normative : `ADR 018 — Postgres en CI sur le substrat runner
  self-hosted Zab` (`ZabLaboratory/Orion`, `docs/adr/018-ci-postgres-substrat-runner-zab.md`,
  §3, §3.1). Implémentation de référence :
  `ZabLaboratory/Orion/.github/workflows/ci.yml:64-137`.

- Tests marqués `@pytest.mark.live` (Quasar) skippés en CI par défaut ;
  exécution manuelle avec les vraies credentials Twitch.

### Lockfile
```

> Note : le dernier item (`@pytest.mark.live`) est répété tel quel en fin de
> bloc pour préserver l'ordre existant du fichier — le nouveau paragraphe
> Postgres s'insère avant lui, pas après.

## Hors scope (rappel §5 de l'issue)

Ne touche pas `D:\Documents\docs\adr\004-cicd-skeleton-etage2.md` (étage 0,
scope G2, non prescriptif pour Zab — tranché par Vigil en révision 4 de l'ADR
018). Aucune modification d'ADR, de droit, de tier ou de gate.

## Vérification post-application (attendue par Eleven)

```
grep -rn "018-ci-postgres" D:\Documents\Zab\agents\_shared\
```
→ doit renvoyer ≥ 1 occurrence.

## Rollback

Fichier hors git (étage 1) : sauvegarder `conventions.md` horodaté avant
écriture (ex. `conventions.md.bak-20260811`) ; rollback = restauration du
backup.
