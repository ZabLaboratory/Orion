# Runbook — `canevas-chat-sponso` écran noir à l'antenne (récidive)

- **Scène** : `canevas-chat-sponso` — `8cbef8cd-ed05-42ca-af9c-7e2762e860b8`
- **Surface** : Orion prod (`orion` / `orion-postgres`, vps-ovh, `zab-internal`)
- **Symptôme** : scène full noir après publish v13 + re-push depuis Prism
- **Occurrences** : 2026-06-29 ~09:38 (incident #1, ancien code Prism) puis ~10:28 (incident #2, **#204 en place**)
- **Gravité** : prod (antenne) — bundle servi non rendable

## Diagnostic (cause racine)

Orion sert à Solar le bundle pointé par `scenes.latest_pushed_version` (chargé
en mémoire au push / au boot via `loadActiveScenes`). Comparaison du bundle
cassé (servi) vs le dernier bon connu :

| | bon (`9456ecf6`, cv `227ccb75`) | cassé (`946b0808`, cv `5fdb693a`) |
|---|---|---|
| `layout` root | `kind:frame`, x0 y0, **1920×1080**, clipsContent | `kind:stack`, `position:{2064,2523}`, **pas de width/height**, sizing fixed |
| clés enveloppe | lsml, **assets**, layout, **operator_inputs** | lsml, layout (assets + operator_inputs **absents**) |
| `blue_blueprint_id` | `34f4b958` | `34f4b958` (préservé) |
| children | 19 | (stack sans dimensions) |

**#204 (`dd9025a`) tournait bien au push de 10:28** — preuve : binding
`34f4b958` **préservé** (fix A) ET racine en coords canvas **2064,2523, NON
normalisée à 0,0** (fix C = revert #195). L'incident #1 de 09:38 était l'ancien
code (binding `""`, cv `0e6f75d4`).

**La cause de l'écran noir est NOUVELLE, applicative, pas opérationnelle** : le
chemin de push (toolbar « Push to Orion ») émet un `layout` racine en **stack
off-artboard sans dimensions** + **enveloppe amputée de `assets` et
`operator_inputs`**, au lieu du `frame` compilé 0,0 / 1920×1080. Solar ne peut
pas mettre en page une racine sans dimensions → noir. La dérive
`canvas_version` (`5fdb693a` vs `227ccb75`) est l'empreinte de ce changement de
forme du layout. #204 fix B (`computeContentOrigin`) ne corrige QUE la vue
éditeur Prism, **pas** le bundle poussé vers Solar → noir persistant malgré
pull main + restart. La query vue v13 (`match_player_champions`) n'est PAS en
cause : le bundle n'atteint jamais un frame rendable, le noir précède tout exec
Blue.

> Verdict : **NOUVELLE cause** (régression sérialisation/compile du chemin de
> push), à corriger en code (Prism `to-scene`/toolbar + bundle compile Orion /
> Solar render des racines off-origin). Pas un correctif opérationnel.

## Restauration (appliquée 2026-06-29 ~10:37)

Backups de la rev cassée AVANT toute action (scratchpad) :
`rev3_black_orion_bundle.json`, `rev3_black_orion_scene.json`,
`rev3_black_zabcanvas_def3.json`.

```bash
# 1. Re-pointer le live vers le dernier bon bundle (frame, assets, opinputs, binding)
GOOD="sha256:9456ecf69f6b4bee4075e98a739be447ec0e1688dad76d54183143342f94c255"
ssh vps-ovh "docker exec orion-postgres psql -U orion -d orion -c \
  \"UPDATE scenes SET latest_pushed_version='$GOOD', updated_at=now() \
   WHERE id='8cbef8cd-ed05-42ca-af9c-7e2762e860b8'\""

# 2. Recharger le roster runtime (loadActiveScenes relit latest_pushed_version)
ssh vps-ovh "docker restart orion"
```

Vérif : `GET /api/v1/health`=200, `/ready`=200, `/api/v1/show`
`active_scene_id=8cbef8cd…`, `latest_pushed_version=sha256:9456ecf6…`.

> Variante propre (sans restart) : `POST /api/v1/scenes/{id}/push`
> `{"rollback_to":"<good sha>"}` (handleRollback — re-point + reload + scene_changed,
> requiert auth operator + version cible validée). Le restart a été retenu car
> l'antenne était déjà noire (blip de reconnexion = amélioration nette).

## Rollback du rollback

Re-pointer sur le sha cassé sauvegardé (`946b0808…`) puis `docker restart orion`.

## Prévention

- Tant que le chemin de push n'est pas corrigé : **ne pas re-pousser cette scène
  depuis la toolbar « Push to Orion »**. Le pull main + restart Prism ne suffit
  PAS (#204 incomplet côté bundle).
- Fix code à porter (hors Keeper → Forge/Conduit) : le push doit émettre le
  `layout` **frame compilé** (origine normalisée OU racine off-origin gérée par
  Solar) + conserver `assets` et `operator_inputs` dans l'enveloppe.

## Prévention (2026-07-02) — guards Orion (`forge/switch-fixes-orion`)

Corrections côté Orion réduisant la surface de cette classe d'incident (le
re-push qui prenait l'antenne immédiatement) et le coût du switch :

- **Guard swap-on-air** (`internal/api/scenes_push.go::surfacePushedVersion`) :
  un re-push d'une scène **active** dont la `scene_version` est **identique à
  celle déjà à l'antenne** est désormais un **no-op silencieux** — aucun
  `LoadExec`, aucun `scene_changed`, aucun `snapshot`. L'antenne ne se recharge
  que sur un **vrai changement de contenu** (scene_version différente). Un
  re-push byte-identique de la scène live ne peut plus provoquer de rechargement
  (test `internal/api::TestSwitchFix_RePushActiveScene_NoReload` + e2e
  `TestE2E_SwitchFix_RePushActiveScene_NoReload`).
- **Idempotence du compile** (`scenes_push.go` + `internal/api/push_dedup.go`,
  `compiler.EnvelopeFingerprint`) : un re-push d'enveloppe identique **skippe le
  compile et tout fetch** Canvas/Blue et réutilise la version déjà persistée.
- **Cache disque des fetches** content-addressed (layout + blueprint graph
  épinglé, `compiler.fetchCache`) : le fetch WAN d'un même hash n'est fait
  qu'une fois.

> Ces guards ne corrigent PAS la cause racine (un push émettait un `layout`
> stack off-artboard sans dimensions, dont la `scene_version` diffère
> réellement — le guard le laisse donc passer). La régression de
> sérialisation/compile du chemin de push reste à corriger côté émetteur
> (Prism `to-scene`/toolbar + rendu Solar des racines off-origin). Le guard
> ferme la classe « re-push redondant/identique prend l'antenne », pas
> « re-push d'un layout cassé ».
