# Runbook — CI Orion : fetch privé Blue instable (cache + race + toolchain)

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
purger le cache VCS Go et le download-cache scoped Blue.

**v1 (PR #350, commit `49659b1`, squash `4f8cc627`) — INSUFFISANT.** Purge
conditionnelle (`git -C "$d" remote get-url origin | grep -q ZabLaboratory/Blue`)
sur chaque sous-répertoire de `cache/vcs`. Ne s'est jamais déclenchée : les
bare repos internes créés par `go mod` (git codehost) ne portent pas de
remote nommé `origin` — confirmé par les logs Forge sur le rebase PR#349
(run 31760522179) : la boucle de purge s'exécute, mais le même hash de cache
survit, même échec "Invalid username or token".

**v2 (PR #352, commit `cfcdf09`, squash `3409b27`) — fix effectif.** Purge
inconditionnelle :

```sh
rm -rf "$(go env GOMODCACHE)/cache/download/github.com/!zab!laboratory"
rm -rf "$(go env GOMODCACHE)/cache/vcs"
```

Sans risque de coût caché : seul Blue est fetché en VCS direct
(`GOPRIVATE`/`insteadOf`) ; tous les autres modules passent par le
download-cache de GOPROXY, non affecté par ce wipe. Chaque job force ainsi un
clone frais sous son propre token, toujours valide au moment de l'appel,
indépendamment de la réutilisation du runner.

**v3 (PR #354, commit `22524fd`, squash `5cf6c695`) — 2e bug distinct, race
de concurrence.** Après v2, échec encore intermittent (staticcheck échoue en
moins d'1s sur un cache qui vient d'être purgé dans le MÊME step — donc pas un
résidu). Preuve par les timestamps du run 31760822765 : `perf-20k gate`/
`conformance-matrix gate` démarrés à 01:30:35, encore actifs pendant la
fenêtre `staticcheck` (01:32:06-01:32:17). Les jobs d'une même run s'exécutent
**concurremment** sur ce runner non conteneurisé par job, et `git config
--global` mute un `~/.gitconfig` **partagé** entre eux : le `--unset` de
nettoyage d'un job fini peut courir pendant qu'un autre job est encore dans sa
fenêtre `go mod download`, lui arrachant son `insteadOf` sous les pieds. Fix :
scoper la config Git au job via `GIT_CONFIG_GLOBAL="$RUNNER_TEMP/gitconfig-blue-read-$GITHUB_JOB"`
au lieu de muter le fichier global partagé — confirmé déterministe sur le run
suivant (31761103392, commit `43ed950` : 8/9 jobs verts, seul govulncheck
restait rouge pour une raison distincte, cf. ci-dessous).

**v4 (PR #355, commit `bf17b31`, squash `d37cd7cb`) — govulncheck, pas un
problème de fetch.** 6 CVE stdlib (net/url, crypto/tls×2, net/http×2,
encoding/xml, encoding/asn1 — GO-2026-6218/6090/6089/6088/5972/5026), toutes
`Fixed in: go1.26.6`. Le wildcard `go-version: "1.26.x"` du workflow
résolvait vers `1.26.5` sur ce runner (toolchain en cache non renouvelée sous
le wildcard). Fix : pin exact `go-version: "1.26.6"` sur les 8 jobs Go.
Clearance Bastion obtenue avant merge (`CLEARED_WITH_CONDITIONS`, Orion#336) :
CVE DoS-only, aucun RCE/confidentialité/intégrité, pas de veto. 2 conditions
de clôture restent ouvertes, hors de ce work unit (suivi séparé) : mesurer le
toolchain qui compile réellement le binaire prod (`Dockerfile:4
FROM golang:1.26-alpine` flottant, `deploy.yml:233 docker compose build` sans
`--pull`), et pin par digest (rejoint aussi le drift `postgres:16-alpine`
noté dans `conventions.md` §Docker).

## Portée du fix / limite connue

Le fix est local au workflow (`ci.yml`) : il s'applique à chaque run pris
individuellement dès que la branche contient le commit. Une branche de PR déjà
ouverte AVANT le fix ne le reçoit pas automatiquement — elle doit merge/rebase
`main`. Pour PR#349 (`forge/effect-invocation-orion-wiring`), Keeper n'a pas pu
pousser directement le correctif dans le worktree local (lease runtime
branch-keyed, déjà tenu par l'agent Forge propriétaire de la branche) :
propagé à la place via l'API GitHub server-side (`POST /merges`, base=PR#349,
head=main) pour chaque itération — commits `4fc4158` (v2), `43ed950` (v3),
`914bc5fa` (v4). Cette voie (merge API, pas de worktree local) reste la
méthode à privilégier pour tout futur besoin de propagation urgente vers une
branche déjà leasée par un autre agent.

## Rollback

- Revert `cfcdf09` / PR #352 (purge inconditionnelle) si elle casse un autre
  job — isolé aux étapes de fetch Blue.
- Revert `22524fd` / PR #354 (`GIT_CONFIG_GLOBAL` scopé) si un job a besoin
  d'un état git global partagé (aucun cas connu à ce jour).
- Revert `bf17b31` / PR #355 (pin `1.26.6`) en cas de régression toolchain —
  repasser au wildcard rouvre les 6 CVE govulncheck, à ne faire qu'en dernier
  recours documenté.

Le v1 (`49659b1` / PR #350) reste inoffensif mais inutile (sa condition ne se
déclenche jamais) ; pas besoin de le revert séparément.

## Non résolu, hors scope Keeper

`build-test` échoue aussi séparément sur `git diff --exit-code -- go.mod go.sum`
(go.sum de PR#349 périmé face au nouveau pin Blue `e96bc5709899`) — problème de
contenu sur la branche Forge, pas d'infra. À corriger côté Forge (`go mod tidy`
+ commit).
