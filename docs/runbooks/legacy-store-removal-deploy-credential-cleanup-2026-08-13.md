# Runbook — nettoyage deploy.yml/Dockerfile + révocation credential orion (2026-08-13)

> Hotfix Keeper, conditions Bastion C3+C1 (clearance PR #346, ADR host-stateless
> cutover / issue #331). Exécuté après le merge de #346 (`f5943ed`).

## Contexte

#346 retire `internal/store`, `migrations/0001-0007` et `datasidecar` (host
stateless cutover, issue #331). Deux effets de bord identifiés par Bastion,
tous deux bloquants avant tout redéploiement :

- **C3** — `Dockerfile` installait `goose` et `COPY`-ait `migrations/` dans
  l'image runtime ; `deploy.yml` lançait `docker compose run --entrypoint
  /goose ... up`. Les deux chemins n'existent plus après #346 — un `COPY` sur
  un dossier absent fait échouer le build immédiatement.
- **C1** — `ORION_SERVICE_REFRESH_TOKEN` (amorce de bootstrap du
  `ServiceTokenManager`, désormais retiré) devenait un credential gelé côté
  ZabAuth : plus personne ne le fait tourner, plus personne n'en est
  propriétaire, encore injecté à chaque deploy (`deploy.yml:148,187`) et
  encore présent dans le `.env` VPS.

## C3 — Dockerfile / deploy.yml

Diagnostic : `Dockerfile:36-42` (`go install goose`) + `:47,54-55` (`COPY
goose` + `COPY migrations /migrations`) ; `deploy.yml:242-246` (step
"Migrate (goose)", `docker compose run --entrypoint /goose orion -dir
/migrations postgres ... up`).

Fix : retrait des deux étapes goose du `Dockerfile` et du step "Migrate
(goose)" de `deploy.yml`. Les étapes DB-up / boot restent inchangées — hors
scope.

Exécution : commit préparé par Forge (`f931aff`, sur `forge/331-legacy-removal`
par-dessus `e6f7528`), poussé par Keeper — l'App Forge n'a pas le scope
`workflows` requis pour toucher `.github/workflows/deploy.yml`, l'App Keeper
si. Push direct du commit existant (`git push origin f931aff:refs/heads/forge/331-legacy-removal`),
aucun nouveau commit créé. Fast-forward `e6f7528..f931aff`. Mergé dans #346
(`f5943ed`).

## C1 — révocation `ORION_SERVICE_REFRESH_TOKEN`

Procédure suivie : `ZabAuth/docs/runbooks/service-token-lifecycle.md` §4
(kill switch) + précédent `orphan-service-token-purge-2026-08-08.md` (identité
d'exécution, appel réseau interne).

1. **Identification de la famille** — SSH `vps-ovh`, `docker exec -i
   zabauth-postgres psql -U zabauth -d zabauth`:
   ```sql
   SELECT family_id, generation, rotated, revoked, refresh_expires_at
   FROM service_tokens
   WHERE service_name='orion' AND NOT revoked
   ORDER BY generation DESC LIMIT 5;
   ```
   → `family_id = da3ee7ad-7253-410d-9d88-77dcae4d1f5c`, tête vivante
   génération 142.

2. **Révocation** — depuis le réseau `zab-internal` (conteneur `zabauth`
   distroless, sans curl : conteneur éphémère `curlimages/curl` attaché au
   même réseau) :
   ```
   POST http://172.26.0.2:4001/api/v1/service-tokens/da3ee7ad-7253-410d-9d88-77dcae4d1f5c/revoke
   X-Authenticated-User: b06508b4-8e54-4532-8848-5a64af08e5e5
   X-Authenticated-Role: operator
   ```
   → `204`. Vérifié en DB : générations 140-142 `revoked=t`,
   `revoked_at=2026-08-13 20:19:02 UTC`.

   Même caveat que le précédent purge du 2026-08-08 : l'identité exécutante
   est assertée par header sur `zab-internal`, pas un JWT opérateur passé par
   ZabGate — traçabilité `token_audit_log` correcte sur l'`actor`, mais la
   provenance gateway-vs-injection-hôte reste indiscernable dans l'audit (cf.
   `orphan-service-token-purge-2026-08-08.md` §3.1).

3. **`.env` VPS** — `/home/ubuntu/orion/.env:32` portait
   `ORION_SERVICE_REFRESH_TOKEN=` (valeur déjà vide). Ligne supprimée
   (`sed -i '/^ORION_SERVICE_REFRESH_TOKEN=/d'`), vérifié absente.

4. **GitHub Secret** — vérifié absent des deux côtés (`gh secret list` sur
   le repo Orion, `gh api .../environments`) : jamais posté à ce niveau,
   seul le `.env` VPS le portait. Rien à supprimer.

Orion n'ayant plus de `ServiceTokenManager` (retiré par #346), aucune
conséquence applicative : le service ne tente plus de rafraîchir cette
famille depuis avant la révocation.

## Rollback

- **C3** : `git revert f931aff` sur `main` restaure les étapes goose — sans
  intérêt tant que `migrations/`/`internal/store` restent supprimés (#346),
  le build recommencerait à échouer sur le `COPY` absent.
- **C1** : révocation non réversible via la route (pas de "un-revoke").
  Si un besoin futur de credential `orion` réapparaît (réintroduction d'un
  store côté Orion), la voie propre est un **nouveau mint** (§2 du runbook
  `service-token-lifecycle.md`), pas une résurrection de la famille
  `da3ee7ad-7253-410d-9d88-77dcae4d1f5c`.

## Vérifications post-op

| Contrôle | Résultat |
|---|---|
| `origin/forge/331-legacy-removal` | `e6f7528..f931aff`, fast-forward |
| PR #346 | mergée, `f5943ed` |
| Famille orion `da3ee7ad-…` | `revoked=t` (3 dernières générations vérifiées) |
| `.env` VPS ligne 32 | absente |
| GitHub Secret `ORION_SERVICE_REFRESH_TOKEN` | inexistant (repo + environments) |

Refs #331.
