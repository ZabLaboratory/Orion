# Résultat du vol — Canary first-flight R9 (2026-06-11)

> Companion au runbook de vol `orion-exec-first-flight-canary.md`. Ce
> document consigne **ce qui s'est passé** — verdict, identifiants, séquence
> exécutée, observables, hotfixes et état final. Owner : Scribe (post-vol).
> Référence : ADR 006 §3.6, issue #106.

---

## Verdict

**FRANCHISSEMENT R9 RÉUSSI.** Première scène exec-bearing à atteindre
l'antenne. Exec live sans incident, sans rollback. Le rollback armé n'a
pas été déclenché.

---

## Identifiants du vol

| Champ | Valeur |
|---|---|
| `CANARY_ID` | `0be9aa3f-a6a9-4d40-8963-7783e92af22f` |
| `CANARY_VERSION` | `sha256:a161d24984f5ea0e6a2ab47df8d3361e9441b2bb76e2d00890b260f79119f743` |
| `BP_CANARY_UUID` | `031fbb29-649e-490d-b7a4-e1558ed6b6e8` (slug `bp-canary`, Blue v1 published) |
| `ROLLBACK_ID` | `f00296c8-c75e-4466-a8c2-41c7816b1d10` (scène roster validée, armée, jamais déclenchée) |
| Date | 2026-06-11 |

---

## Séquence exécutée

1. **Rollback armé et validé** — scène connue-bonne (`ROLLBACK_ID`) sur l'antenne,
   vérifiée `validated` avant l'ouverture de la fenêtre.
2. **Blueprint canary créé dans Blue** — `POST /blue/api/v1/blueprints` → `201` ;
   graph mis à jour (`PUT /versions/…`) ; version v1 publiée.
   UUID retourné : `BP_CANARY_UUID = 031fbb29-649e-490d-b7a4-e1558ed6b6e8`.
3. **Push canary** — `POST /orion/api/v1/scenes/0be9aa3f.../push` avec payload :

   ```json
   {
     "canvas_version": "<hash-layout-réel>",
     "blue_blueprint_id": "031fbb29-649e-490d-b7a4-e1558ed6b6e8"
   }
   ```

   Réponse : `200`, `scene_version = sha256:a161d24…`, 0 erreur.

   > **Piège doc corrigé** : le payload de push prod utilise l'**UUID réel**
   > retourné par Blue à la création du blueprint, pas le slug `"bp-canary"`.
   > `bp-canary` n'est que le label humain (slug). Voir §Pièges ci-dessous.

4. **Gate négatif prouvé** — `POST /show/active-scene {scene_id: CANARY_ID}`
   avant validate → `409 SCENE_NOT_VALIDATED`. Le gate a refusé comme attendu.
5. **Validate** — `POST /scenes/0be9aa3f.../validate` → `202`, état → `validated`.
6. **Activate** — `POST /show/active-scene {scene_id: CANARY_ID}` → `200`.
   Exec live.

---

## Observables (preuve)

| Observable | Valeur |
|---|---|
| `orion_task_cpu_seconds_total{scene_id=canary}` | Absent à T-0 (normal — série Prometheus non émise avant la première tâche) ; apparaît après activation et grimpe `0.039 → 0.114 → 0.115 s` |
| Log `show` 13:11:31 | `canary first-flight: maintainers loop complete (counter=9)` — boucle bornée atteinte |
| `exec_completion_rejected_total` | 0 |
| `exec_resume_stale_total` | 0 |
| `parked_tasks` | 0 |
| Load host | Redescend après activation — aucune saturation |
| Conteneurs | Tous healthy |
| `GET /health` | 200 tout au long du vol |

---

## Hotfix appliqué en vol

**`canvas_version` : remplacement du stub `"v1"` par un hash de layout réel.**

Le payload initial portait `"canvas_version": "v1"` (valeur issue du stub e2e),
qui a fait échouer le push prod avec `FETCH_UPSTREAM / Layout not found 404` —
Canvas appelé sur `GET /canvas/api/v1/layouts/v1` n'a pas trouvé le layout.

Correction en vol : la `canvas_version` a été remplacée par le hash sha256 du
layout Canvas réel de la scène rollback (`7e6362a2…`), déjà existant côté Canvas.
Le push a alors répondu `200` et le vol a pu démarrer.

Voir §Pièges ci-dessous pour la doctrine complète.

---

## État final

La scène canary est **laissée à l'antenne comme canary de régression permanent**.
Elle exerce le scheduler (boucle, delay, variables, live ops) à chaque run et
constitue la baseline observée pour le calibrage des seuils d'alerte #89 :

- `orion_task_cpu_seconds_total` max observé : `0.115 s`
- `orion_event_shed_total` : 0
- `orion_parked_tasks` : 0

---

## Pièges corrigés — production vs stub e2e

Ces deux clés du payload de push ont des valeurs **différentes entre les tests
e2e et la production**. Le stub e2e en mémoire utilise des littéraux courts qui
n'existent jamais dans un environnement réel. Ne pas les copier dans un payload
prod.

### Piège 1 — `blue_blueprint_id` : UUID réel, pas le slug

| Contexte | Valeur correcte |
|---|---|
| **Test e2e Orion** (`tests/e2e/`) | Littéral arbitraire comme `"bp-canary"`, `"bp-loop"`, `"bp-1"` — c'est une clé de stub-fetcher en mémoire (`FetchBlueprint` est mocké ; la valeur n'est jamais résolue contre Blue réel). Ces chaînes peuvent être n'importe quoi de stable pour le test. |
| **Push prod** | L'**UUID réel** retourné par Blue à la création du blueprint : `POST /blue/api/v1/blueprints` → `{"blueprint_id": "031fbb29-..."}`. C'est cet UUID qui est passé dans `blue_blueprint_id`. |

Le slug `bp-canary` est le `slug` / `blueprint_name` humain, **pas** l'id de
résolution. Le fetcher prod appelle
`GET /blue/api/v1/blueprints/{blue_blueprint_id}` — avec un slug ça retourne 404
(les blueprints sont résolus par UUID, pas par slug, sur cet endpoint).

### Piège 2 — `canvas_version` : hash de layout Canvas, pas `"v1"`

| Contexte | Valeur correcte |
|---|---|
| **Test e2e Orion** (`tests/e2e/`) | Littéral `"v1"` — stub-fetcher en mémoire pour `FetchCanvasLayout`, la valeur n'est jamais résolue contre Canvas réel. |
| **Push prod** | Un **hash sha256 de layout** servi par Canvas sur `GET /canvas/api/v1/layouts/{hash}`. Ce hash est retourné par Canvas quand la scène est sauvegardée/publiée côté Canvas. Exemple en vol : `7e6362a2…` (hash du layout de la scène rollback). |

**Dépendance Orion ← Canvas** : le layout identifié par `canvas_version` doit
**pré-exister dans Canvas avant le push**. Si le layout n'existe pas, le compilateur
Orion retourne `FETCH_UPSTREAM / Layout not found 404` à la création de la version.

> L'alignement fin du contrat `canvas_version` (format du hash, cycle de vie
> Canvas → Orion) relève de **Conduit**. Ce runbook documente le piège opérationnel
> constaté en vol ; la spécification formelle du contrat est dans
> `docs/contracts/` ou un ADR dédié.
