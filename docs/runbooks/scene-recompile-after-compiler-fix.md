# Runbook : Recompile/repush de scènes après fix compilateur

> Scope Orion. Délégué par Keeper à Scribe pour la mise en forme.
> Contenu technique = Keeper. Source des routes = `internal/api/public.go`.
> Source des handlers = `internal/api/scenes_push.go`.

---

## Symptôme

Un fix compilateur ne s'applique **pas** aux scènes déjà poussées. Leur graphe
compilé (`graph_json` / `bundle_json` dans `scene_pushed_versions`) est figé par
l'ancienne version du compilateur au moment du push d'origine.

Diagnostic : un graphe stale est la signature d'un push antérieur au fix.
La `scene_version` (hash du contenu compilé) ne change que si le compilateur
produit un résultat différent — elle reste identique tant que la scène n'est
pas re-pushée.

---

## Procédure

### 1. Identifier les scènes concernées

Lister les scènes dont le graphe a été poussé avant le commit qui corrige le
compilateur. La `scene_version` courante s'obtient via :

```
GET /orion/api/v1/scenes/{id}/graph
```

Le champ `scene_version` dans la réponse identifie la version active.

### 2. Rejouer l'enveloppe de push

Le handler `pushScene` (`internal/api/scenes_push.go`) **ne recompile pas depuis
le stockage** — il recompile depuis l'enveloppe que l'appelant fournit dans le
corps. Il faut donc rejouer l'enveloppe originale avec les valeurs réelles
issues de `scene_definitions` :

```
POST /orion/api/v1/scenes/{id}/push
Content-Type: application/json

{
  "canvas_version": "<valeur canvas_version de scene_definitions>",
  "blue_blueprint_id": "<valeur blue_blueprint_id de scene_definitions>",
  "components": [...]
}
```

> Si la scène utilise le champ `blueprints[]` (ADR 001 §3.1, post-multi-blueprint),
> utiliser `blueprints` à la place de `blue_blueprint_id`. Les deux champs sont
> mutuellement exclusifs — les fournir ensemble retourne `400
> ENVELOPE_BLUEPRINT_CONFLICT`.

Les valeurs `canvas_version` et `blue_blueprint_id` / `blueprints` proviennent
de la table `scene_definitions` (dernière ligne pour la scène).

### 3. Valider la scène recompilée

La validation est requise avant que la scène puisse être activée à l'antenne
(gate ADR 003 §3.2.2). Lancer la campagne :

```
POST /orion/api/v1/scenes/{id}/validate
```

Réponse `202` avec `(scene_id, scene_version, harness_version)`. Interroger le
résultat :

```
GET /orion/api/v1/scenes/{id}/validation
```

Attendre `status: validated`.

### 4. Re-activer la scène active

Si la scène était active avant la procédure, la re-pointer :

```
POST /orion/api/v1/show/active-scene
Content-Type: application/json

{ "scene_id": "<uuid>" }
```

> `postActiveScene` (`internal/api/show.go`) gate l'activation sur la validation
> : une scène non validée pour le `harness_version` courant est refusée.

---

## Preuve de recompilation

- La `scene_version` retournée par le push **change** par rapport à l'ancienne
  (le hash est sensible au contenu compilé — si le fix modifie la structure du
  graphe, le hash diffère).
- Inspecter le graphe expansé via `GET /api/v1/scenes/{id}/graph` et vérifier
  que les noeuds attendus sont présents et câblés (ex. : absence de noeud
  `on-start` parasite, présence des `core.input` promus par ADR 014).

> Si la `scene_version` est identique avant/après repush, le compilateur a
> produit le même résultat — vérifier que le bon binaire est déployé.

---

## Rollback

Si la nouvelle version pose problème, re-pointer sur la version précédente
**sans recompiler** via le handler `handleRollback` :

```
POST /orion/api/v1/scenes/{id}/push
Content-Type: application/json

{ "rollback_to": "<scene_version cible>" }
```

`handleRollback` (`internal/api/scenes_push.go`) :
1. Charge la `ScenePushedVersion` existante depuis `scene_pushed_versions`.
2. Vérifie que la version cible est **validée** pour le `harness_version` courant
   (gate B-rollback ADR 003 §3.2.2) — un rollback vers une version non validée
   est refusé avec `409 SCENE_NOT_VALIDATED`.
3. Re-pointe `latest_pushed_version` via `SetLatestPushedVersion` (transaction).
4. Recharge le runtime avec les artefacts existants (pas de recompilation).

Les anciennes versions restent en base dans `scene_pushed_versions` (pas de
suppression automatique) — le rollback est donc disponible tant que la version
n'a pas été purgée par une purge d'archive.

---

## Contexte décisionnel

| ADR | Section | Décision |
|---|---|---|
| ADR 002 | §3.1 | upsert-on-push : le handler crée la scène à la volée si absente |
| ADR 003 | §3.2.2 B-rollback | validation gate sur push-swap live et rollback |
| ADR 006 | §3.4 | R9-lift : exec installé au push/rollback si validé |
| ADR 014 | — | fix `core.input` lisant `__vars..` à l'expansion de reference (cause racine des graphes stale de juin 2026) |
