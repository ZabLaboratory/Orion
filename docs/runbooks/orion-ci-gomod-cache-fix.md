# Runbook — CI Orion : cache VCS Go pollué par un token App révoqué

## Symptôme

3+ jobs Go de `ci.yml` (staticcheck, golangci-lint, perf-20k gate, govulncheck…)
échouent en parallèle sur l'étape `fetch private Go module` avec :

```
go: github.com/ZabLaboratory/Blue/runtime/go@vX.Y.Z: invalid version:
git ls-remote -q --end-of-options origin in .../pkg/mod/cache/vcs/<hash>: exit status 128:
	remote: Invalid username or token. Password authentication is not supported for Git operations.
	fatal: Authentication failed for 'https://github.com/ZabLaboratory/Blue/'
```

Le job `build-test` (généralement le premier de la run) réussit ; les jobs
suivants échouent tous à l'identique quelques secondes/minutes après.

## Cause racine

Le runner self-hosted `vps-ovh` (label `vps-ovh`, conteneurs `jit-orion-*`)
est réutilisé **séquentiellement** pour tous les jobs d'une même run — malgré
le commentaire du workflow supposant un runner éphémère par job. Le cache
module Go (`~/go/pkg/mod/cache/vcs/<hash>`) persiste donc entre jobs de la
même run et retient un clone bare de `ZabLaboratory/Blue` dont le remote
`origin` embarque le token App scoped **du premier job**. `create-github-app-token`
révoque ce token automatiquement en fin de job (`skip-token-revoke: false`).
Tout job suivant qui réutilise ce clone en cache échoue donc l'authentification,
même si son propre token fraîchement minté est valide.

Diagnostic confirmé en direct sur le runner (2026-08-14) : `docker inspect`
sur le conteneur `jit-orion-*` en cours ne montre aucun mount persistant côté
hôte — le cache vit uniquement dans le filesystem du conteneur, mais ce
conteneur sert bien plusieurs jobs à la suite (pas un conteneur par job).

## Fix

`.github/workflows/ci.yml` — dans les 8 jobs Go, avant chaque `go mod download` :
purger tout clone en cache dont le remote `origin` référence
`ZabLaboratory/Blue`, plus le download-cache correspondant :

```sh
rm -rf "$(go env GOMODCACHE)/cache/download/github.com/!zab!laboratory"
for d in "$(go env GOMODCACHE)"/cache/vcs/*/; do
  git -C "$d" remote get-url origin 2>/dev/null | grep -q ZabLaboratory/Blue && rm -rf "$d"
done
```

Chaque job force ainsi un clone frais sous son propre token, toujours valide
au moment de l'appel, indépendamment de la réutilisation du runner.

PR #350 (merge `main`, commit `49659b1`), squash-mergée `4f8cc627`.

## Portée du fix / limite connue

Le fix est local au workflow (`ci.yml`) : il s'applique à chaque run pris
individuellement dès que la branche contient ce commit. Une branche de PR déjà
ouverte AVANT le fix ne le reçoit pas automatiquement — elle doit merge/rebase
`main`. Pour PR#349 (`forge/effect-invocation-orion-wiring`), Keeper n'a pas pu
pousser directement le correctif : le lease de worktree runtime est
branch-keyed et déjà tenu par l'agent Forge propriétaire de cette branche.
Propagation laissée au porteur/Forge (merge ou rebase de `main`).

## Rollback

Revert du commit `49659b1` / PR #350 sur `main` si le purge s'avère
insuffisant ou casse un autre job — aucune dépendance croisée, changement
isolé aux étapes de fetch du module privé Blue.

## Non résolu, hors scope Keeper

`build-test` échoue aussi séparément sur `git diff --exit-code -- go.mod go.sum`
(go.sum de PR#349 périmé face au nouveau pin Blue `e96bc5709899`) — problème de
contenu sur la branche Forge, pas d'infra. À corriger côté Forge (`go mod tidy`
+ commit).
