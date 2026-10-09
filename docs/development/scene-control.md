# Sélection déclarative de scènes

Orion possède l'admission et l'exécution Blue ; Solar possède la préparation et
la présentation Vision. Program utilise `solar/program`, Preview utilise
`solar/preview`. Le petit LSML de sélection vit sous `/scene_control` dans la
ressource native `orion/state`.

Dans desired/observed, `scene_version` identifie l'ensemble admis
`artifact_set_digest`, comme la clé du catalogue. Dans les documents complets
`solar/program` et `solar/preview`, `scene_version` conserve l'adresse LSML
publiée par Canvas ; `x-orion-artifact-set` transporte séparément l'identité
admise. Solar acquiert cette adresse LSML et l'utilise dans son acquittement.
Le manifeste Blue porte lui aussi l'adresse LSML, plus le revision_id signé.
Ces identités peuvent être différentes et ne doivent jamais être substituées.

```json
{
  "lsml": "1.2",
  "schema": "orion.scene-control.v1",
  "scene_id": "orion-scene-control",
  "layout": {"type": "frame", "children": []},
  "defaults": {
    "desired": {
      "program": {"scene_id": "scene-id", "scene_version": "sha256:version", "stream_id": "stream-id"},
      "preview": {"scene_id": "scene-id", "scene_version": "sha256:version", "stream_id": "stream-id"}
    },
    "observed": {}
  }
}
```

Préparer d'abord une capsule signée via
`POST /api/v1/runtime/scene-catalog`, avec le même corps et la même autorité que
scene-intent : `take-on-air` pour Program, `prepare-preview` pour Preview.
La préparation vérifie les octets sans activer ni exécuter Blue. Le catalogue
ne produit aucun bundle de rendu. Solar acquiert/cache séparément LSMLZ et assets.

Pour changer de scène, appliquer une mutation native absolue à
`/scene_control/defaults/desired/program` ou `preview`, avec les trois identités
exactes du catalogue. Orion attribue un identifiant d'activation frais lorsque
la sélection change. Rejouer la même sélection ou modifier `observed` n'exécute
pas de nouveau on-start. A → B → A produit une nouvelle activation de A.
Retirer une entrée desired ne constitue pas une commande de libération ; la
scène courante reste occupante. Une sélection valide est nécessaire pour changer.

Orion publie `preparing`, `active`, `failed` ou `superseded`, avec
`activation_id`, `scene_id`, `scene_version` et une raison en cas d'échec.
`active` suit l'acquittement Solar et le commit de l'instance Blue. Un
document invalide est refusé et journalisé, sans interprétation partielle.

La coordination utilise `x-solar-transition` uniquement dans la ressource de
scène en RAM : request_id, phase prepare/commit/finalize/abort et source candidate.
Solar prépare un frame caché, conserve l'ancien pour compensation et écrit son
acquittement dans `/solar_presentation_program` ou `preview` de `orion/state`.
L'ancien frame est libéré lorsque la métadonnée de transition disparaît après
finalisation. Les Blue sont bloquées pendant cette fenêtre ; aucune nouvelle
Blue ne s'exécute lors d'une préparation refusée. Les affectations caméra sont
retenues puis projetées sur la scène finalement choisie.

Chemins configurables : `ORION_SCENE_CONTROL_PATH`, `ORION_SCENE_CACHE_PATH`.
Par défaut, sous-dossier Orion séparé par owner/tenant/utilisateur, près du fichier
d'intention stream, sinon de l'état du service, sinon de l'asset root. Aucun secret
de handshake ni LSML live muté ne figure dans ces fichiers. La reprise depuis
le disque requiert toujours des attestations encore valides. Un arrêt total
reconstruit depuis les sources immuables et la sélection souhaitée ; il ne
restaure pas les derniers résultats dynamiques Blue.

L'intégration d'un émetteur Prism reste hors de ce changement. Le contrat est
déjà consommable par un client LSDP local autorisé. Aucune modification Prism.

## Editable Preview lifetime

Preview owns one current authoring clone. A regular scene activation or an
external Blue Preview commit releases the prior authoring runtime and its wire
entry. Returning to that scene admits a fresh authoritative head; the legacy
activate endpoint only validates an already-current head and cannot restore an
inactive scene. Current leaf edits retain ordered LSDP delivery.

An explicitly promoted on-air generation is a separate active consumer. At most
one such editable clone is retained; its replacement stops the former Program
runtime. Preview switches never stop that active Program generation. Shutdown
stops both owners once. Tests: internal/runtime/preview_editable_test.go.
