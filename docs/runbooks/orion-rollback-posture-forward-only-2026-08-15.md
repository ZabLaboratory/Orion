# Runbook — posture de rollback Orion : `FORWARD_ONLY` (2026-08-15)

> ADR-BLUE-012 R6 (Blue), §9 clause 334 : *« Après destruction des prérequis
> legacy, aucun simple redéploiement d'artefact n'est présenté comme
> rollback. Le runbook indique explicitement l'état `LEGACY_RESTORABLE` ou
> `FORWARD_ONLY`, le propriétaire de décision, la sauvegarde, le RTO/RPO,
> les commandes de restauration ou revert, et le test PGM, sans aucun
> secret. »* Ce document répond à cette exigence pour Orion, dans le cadre
> de l'axe « rollback » d'Orion#335 (`B3-R6-COMPAT-ORION`).
>
> Complète, sans le remplacer, `legacy-store-removal-deploy-credential-cleanup-2026-08-13.md`
> (le hotfix C1/C3 qui a suivi le retrait du store).

## Contexte

PR #346 (`f5943ed`, 2026-08-13, « final #15 cutover ») a retiré
`internal/store/` et `migrations/` de `main` — **avant** que le point de
non-retour ADR §9 (Orion#340, `B3-R6-19-ORION`) ait été exécuté. Orion#340
reste `OPEN`/`status:queued`/0 commentaire à la date de ce runbook : aucun
exercice de rollback (§9 items 10-11), aucune preuve PGM, aucune bascule
`FORWARD_ONLY` formelle n'avait été faite avant le retrait de code. C'est
l'écart que ce runbook corrige a posteriori, en déclarant l'état réel
plutôt qu'en le laissant implicite.

## 1. État déclaré : `FORWARD_ONLY`

**C'est un choix assumé, pas un constat de destruction.** Au moment de ce
runbook, le résidu legacy **existe toujours** en production :

```
$ ssh vps-ovh docker ps -a --filter name=orion
NAMES                           STATUS                   IMAGE
orion                           Up 3 minutes (healthy)   orion-orion
orion-workload-identity-agent   Up 4 hours (healthy)     zabgate-workload-identity-agent
orion-postgres                  Up 29 hours (healthy)    postgres:16-alpine

$ ssh vps-ovh docker volume ls | grep orion
local     orion_pg_data                                    1    70.2MB

$ ssh vps-ovh docker exec orion-postgres psql -U orion -d orion -c '\dt'
 assets | goose_db_version | scene_definitions | scene_pushed_versions |
 scene_validations | scenes | service_token_state |
 show_blueprint_stream_rules | show_state | show_stream_rules   (10 rows)

$ … SELECT count(*) FROM scenes / scene_pushed_versions / scene_definitions / show_state / service_token_state;
 scenes=95   scene_pushed_versions=250   scene_definitions=383
 show_state=1   service_token_state=1   assets=0

$ … SELECT max(updated_at), max(created_at) FROM scenes;
 max_scenes_updated = 2026-08-13 04:30:56.16622+00
```

Le schéma et les données sont donc **intacts et en principe restaurables**,
mais **jamais exercés ni scannés** (aucun exercice §9 item 10/11, aucun
scan négatif clause 331 — voir §8). `FORWARD_ONLY` est déclaré malgré ça,
pour les raisons du §2 — pas parce que le résidu n'existe pas.

## 2. Pourquoi ce n'est pas une restauration crédible

1. **Aucune image Docker legacy en cache.** Seule `orion-orion:latest`
   (build stateless du jour) existe sur le VPS — `docker compose build`
   écrase la même étiquette à chaque deploy, rien n'est conservé par
   version. Une restauration exigerait un **rebuild** depuis un commit
   pré-#346, pas un redéploiement d'artefact existant.
2. **Le binaire ainsi reconstruit serait fonctionnellement amputé.**
   `ORION_SERVICE_REFRESH_TOKEN` (famille `da3ee7ad-…`) a été révoqué de
   façon irréversible le 2026-08-13 20:19:02 UTC (runbook 2026-08-13 §C1 ;
   confirmé structurellement : `ZabAuth/docs/runbooks/service-token-lifecycle.md`
   §4, kill switch idempotent, aucune route de dé-révocation dans l'API).
   Les effets qui en dépendaient (`internal/auth/service_token.go`,
   `internal/effects/dbquery.go`, `dbschema.go`, `servicecall.go`,
   `internal/compiler/http_fetcher.go` — soit `core.db.*` et
   `core.http.request@1` cross-service) resteraient morts même sur un
   rebuild réussi.
3. **Les données sont figées au 2026-08-13 04:30:56 UTC** et se dégradent
   chaque jour (aucune écriture depuis). Un rollback aujourd'hui perdrait
   tout ce qui a suivi cette date sur les scènes/shows.
4. Aucun exercice §9 items 10-11 n'a jamais été mené (pas d'environnement
   legacy séparé, pas de rollback prouvé avec PGM observé avant retrait).
   Il n'existe donc **aucune preuve** que ce chemin fonctionne, même en
   ignorant 1-3.

Conclusion : un rebuild legacy serait un artefact dégradé, sur données
obsolètes, jamais validé — pas un filet de sécurité crédible. Le seul
recours réel est le chemin stateless (§6).

## 3. Propriétaire de décision

**Porteur** (compte GitHub `ClodoCapeo`). Décision prise dans le fil de
travail Orion#335 le 2026-08-15 : abandon explicite du rollback legacy au
profit d'un revert/forward-fix stateless — la branche que l'ADR §9 item 13
prévoit nommément (*« décision humaine explicite abandonnant le rollback
legacy »*), prise **pendant que l'option était encore ouverte** (résidu
non détruit), pas après coup. Relayée par team-lead, exécutée et
documentée par Keeper. Voir `AGENT_CHECKPOINT` sur Orion#335 (ce work
unit) pour la trace horodatée.

## 4. Sauvegarde

**Aucune sauvegarde formelle n'existe.** Vérifié sur `vps-ovh` :

```
$ ssh vps-ovh "crontab -l 2>/dev/null | grep -i 'orion\|pg_dump\|backup'; \
  find / -maxdepth 4 -iname '*orion*backup*' -o -iname '*orion*dump*'"
(aucune sortie)
```

Ni cron, ni `pg_dump` planifié, ni fichier de backup pour Orion. Le seul
artefact existant est le conteneur `orion-postgres`/volume `orion_pg_data`
lui-même (§1) — ce n'est **pas** une sauvegarde désignée, c'est un résidu
non intentionnel voué à la destruction sous Orion#340 (§8). Aucune
sauvegarde n'est créée par ce runbook : la décision `FORWARD_ONLY` acte
que ce résidu n'a pas vocation à être préservé au-delà de la fenêtre
`FORWARD_ONLY` → destruction/scan négatif d'Orion#340.

## 5. RTO / RPO — chemin stateless (le seul recours réel)

**RTO** (redéploiement depuis un commit connu-bon via `deploy.yml`,
trunk-based, sans étape DB/migration résiduelle) — mesuré sur les 3
derniers déploiements réussis (`gh run list --workflow=deploy.yml
--status=success`, `createdAt`→`updatedAt`) :

| Run | Créé | Terminé | Durée |
|---|---|---|---|
| 31905990047 | 20:10:55Z | 20:11:39Z | 44 s |
| 31904924780 | 19:47:50Z | 19:49:02Z | 72 s |
| 31902274746 | 18:51:17Z | 18:52:38Z | 81 s |

≈ **45-80 s** de bout en bout (build avec cache BuildKit + healthcheck
interne 15×3s max + smoke gateway 8 tentatives). Pas d'étape de migration
à rejouer (`goose` retiré du Dockerfile et de `deploy.yml`).

**RPO** — ne se lit pas comme une perte de données DB : Orion ne persiste
plus aucun état. `cmd/orion/main.go` (commentaires « Cold-start scene
reseed — RETIRED (#15, #331) ») : **le roster boot vide**, sans reseed ;
les slots preview/on-air ne se peuplent que via la surface scene-intent
(Prism → Gate → Orion, Prepare/Take attesté depuis ZabCanvas, autorité
durable — ADR §7). Conséquence opérationnelle concrète : **après tout
restart/redéploiement Orion, l'opérateur (Prism) doit ré-émettre
Prepare/Take pour la scène qui doit repasser à l'antenne** — ce n'est pas
automatique. C'est le vrai « RPO » du chemin stateless : zéro perte de
données (ZabCanvas reste la source de vérité), mais une action manuelle
de reprise après tout redémarrage.

## 6. Commandes de revert / forward-fix (chemin stateless uniquement)

Aucun rollback vers `orion-postgres`/legacy. Le seul recours si un commit
sur le chemin stateless casse la prod :

```bash
# 1. Revert trunk-based standard (jamais de force-push, jamais --no-verify)
git revert <bad_commit_sha>
git push origin <branch>
# PR, CI verte (ci.yml), merge sur main

# 2. Le merge déclenche deploy.yml automatiquement (push sur main).
#    Suivre : gh run watch (broker) ou gh run view --log-failed en cas d'échec.

# 3. Après un déploiement réussi (santé/smoke déjà automatiques dans
#    deploy.yml), reprise manuelle obligatoire côté opérateur :
#    ré-émettre Prepare/Take (Prism) pour chaque scène qui doit être
#    à l'antenne — cf. §5, aucun reseed automatique.
```

Aucune étiquette d'image n'est conservée sur le VPS (`docker images`
n'affiche qu'`orion-orion:latest`, réécrasée à chaque build) : un
« rollback » signifie toujours *revert de commit + rebuild via
`deploy.yml`*, jamais un simple swap d'image.

## 7. Test PGM

Le healthcheck interne (`GET /api/v1/health`) et le smoke test gateway
déjà présents dans `deploy.yml` sont **nécessaires mais pas suffisants**.
Conformément à la règle d'or de `agents/_shared/live-testing.md` (*« le
seul critère valide d'un test Blue = un vrai stream Twitch montrant le
résultat à l'antenne »*), tout revert/forward-fix touchant le chemin
d'exécution live (scene-intent, projection LSDP/Solar) doit être validé
par un enregistrement `.mp4` local (OBS/Pulsar) analysé `ffprobe` +
`ffmpeg` (spatial stddev + niveaux de luma) avant d'être considéré
résolu — un screenshot CEF ou un snapshot LSDP seuls ne suffisent pas.

## 8. Résidu légué à Orion#340 — à ne pas retraiter ici

Cette unité (#335) ne touche pas et ne ferme pas #340. Ce que #340 hérite
explicitement, pour ne pas le redécouvrir en incident :

- **Conteneur `orion-postgres`** (up/healthy) et **volume `orion_pg_data`**
  (70.2 MB) — à révoquer/détruire ou rendre cryptographiquement inutilisable
  (ADR §9 item 14), avec preuve.
- **10 tables** avec données réelles gelées au 2026-08-13 04:30:56 UTC
  (95 `scenes`, 250 `scene_pushed_versions`, 383 `scene_definitions`, 1
  `show_state`, 1 `service_token_state`, 0 `assets`) — périmètre du scan
  négatif clause 331.
- **`service_token_state.refresh_token_enc`** — 1 ligne résiduelle,
  chiffrée (Fernet), liée à la famille déjà révoquée `da3ee7ad-…`. Le
  **scan négatif de la clause 331 n'a jamais été exécuté** sur cette
  ligne ni sur le reste de la base — #340 est l'unité qui doit le faire
  (« scan négatif couvre runbook, logs, rapports, captures, artefacts CI
  et sauvegardes accessibles »).
- **`docker-compose.prod.yml` / `deploy.yml`** référencent encore
  `orion-db` (§9 ci-dessous) — le retrait DB/runtime/credentials scope
  d'Orion#340 couvre logiquement ce nettoyage une fois le résidu purgé.

## 9. Constat opérationnel : dépendance de démarrage sur `orion-db` (constaté, non corrigé — scope #340)

`docker-compose.prod.yml` (service `orion`) :

```yaml
depends_on:
  orion-db:
    condition: service_healthy
  orion-workload-identity-agent:
    condition: service_started
```

Et `deploy.yml` (« Build, migrate, restart »), à **chaque** déploiement,
sans exception ni condition sur ce qui a changé :

```bash
echo "=== Ensure DB is up ==="
docker compose -f docker-compose.prod.yml up -d orion-db
for i in $(seq 1 30); do
  if docker compose -f docker-compose.prod.yml ps orion-db | grep -q healthy; then break; fi
  sleep 2
done
# … pas de test explicite du résultat de la boucle …
echo "=== Up services (single boot) ==="
docker compose -f docker-compose.prod.yml up -d orion-db
docker compose -f docker-compose.prod.yml up -d --force-recreate orion
```

**Risque réel** : le binaire `orion` ne touche plus `orion-postgres` (code
`internal/store` retiré), mais `deploy.yml` continue de (re)lancer
`orion-db` à chaque deploy et d'attendre son état `healthy` (jusqu'à 60 s,
sans échec explicite si le délai expire). L'étape suivante,
`docker compose up -d --force-recreate orion`, respecte `depends_on:
condition: service_healthy` au niveau Compose : si `orion-db` n'est pas
sain à ce moment, Compose refuse de démarrer `orion` et retourne un code
non nul, ce qui — sous `set -euo pipefail` — **fait échouer tout le script
de déploiement**. Concrètement : **si `orion-postgres` tombe ou est
retiré sans que `depends_on` soit également nettoyé, plus aucun déploiement
Orion ne passe** — y compris un déploiement qui ne touche à rien lié à la
DB. C'est un couplage de démarrage résiduel et silencieux, pas un filet de
sécurité voulu. Non corrigé ici (portée de ce runbook = déclarer la
posture, pas modifier la config prod) — signalé pour qu'Orion#340 le
retire avec le reste du résidu (§8).

## Vérifications post-op

| Contrôle | Résultat |
|---|---|
| `docker ps -a --filter name=orion` (vps-ovh) | `orion` healthy 3 min, `orion-postgres` healthy 29 h |
| `docker volume ls` / `docker system df -v` | `orion_pg_data` 70.2 MB |
| `\dt` sur `orion` (Postgres) | 10 tables legacy intactes |
| Compte lignes (scenes/pushed_versions/definitions/show_state/service_token_state) | 95 / 250 / 383 / 1 / 1 |
| `max(scenes.updated_at)` | 2026-08-13 04:30:56 UTC (figé) |
| `docker images` (vps-ovh, filtre `*orion*`) | 1 seule image : `orion-orion:latest`, build du jour |
| `crontab -l` + recherche fichiers backup (vps-ovh) | aucune sauvegarde Orion |
| Famille `ORION_SERVICE_REFRESH_TOKEN` (`da3ee7ad-…`) | `revoked=t`, 2026-08-13 20:19:02 UTC (runbook 2026-08-13) |
| Orion#340 (`B3-R6-19-ORION`) | `OPEN`, `status:queued`, 0 commentaire |
| RTO mesuré (3 derniers runs `deploy.yml` réussis) | 44 s / 72 s / 81 s |

## Hors de portée de ce runbook

Aucun `docs/adr/` modifié. Aucune issue fermée (#335 reste ouverte pour
son axe compat ; #340 reste ouverte et n'est pas traitée ici). Aucune
restauration, aucun revert de #346, aucune modification de
`docker-compose.prod.yml`/`deploy.yml`, aucun redeploy, aucun restart.
Aucun secret : uniquement des noms, statuts (`revoked`/`healthy`/`OPEN`)
et compteurs — aucune valeur de credential, token ou mot de passe.

Refs #331, #335, #340. ADR-BLUE-012 (Blue) §9, §7, §13 (B19/B20), clause 334.
