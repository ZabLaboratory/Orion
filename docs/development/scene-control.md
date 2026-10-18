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

## Libérer le Program après arrêt de diffusion

Le contrôleur desktop confirme l'arrêt réel des sorties avant cette commande.
Orion reçoit ce constat local, sans interroger lui-même Pulsar. Les deux moteurs
Solar physiques persistent ; seul le document et son auteur Program sont retirés.
Le Preview actif, ses écritures et son identité restent inchangés.

Après règlement des écritures en cours, demander une identité coordonnée en
appliquant à `/scene_control/defaults/inspect_program` sur `orion/state` :

```json
{
  "schema": "orion.program-inspect.v1",
  "command_id": "fresh-inspection-id",
  "runtime_instance_id": "current-pulsar-runtime",
  "take_command_id": "last-confirmed-take",
  "expected_role_map": {"on_air": "B", "preview": "A"},
  "expected_revisions": {"program": 1, "preview": 5, "role_map": 1}
}
```

`/scene_control/defaults/observed/inspect_program` recopie exactement la requête
et ajoute `status: active`, `target: solar/lane-b`, `scene_id`, `scene_version`,
`artifact_set`, ainsi que `writer_id` et `edit_seq` si l'auteur est éditable.
L'identité est lue sous le bail Program après drainage des inputs acceptés et
acquittement natif. Elle n'active ni ne compile une scène. Un refus renvoie
`status: refused` et `reason`, sans identité exploitable. La nouvelle inspection
utilise un nouveau command_id ; aucun état supplémentaire n'est publié à chaque edit.

Puis appliquer à `/scene_control/defaults/release_program` :

```json
{
  "schema": "orion.program-release.v1",
  "command_id": "fresh-release-id",
  "stop_command_id": "confirmed-output-stop-id",
  "output_state": "stopped",
  "runtime_instance_id": "current-pulsar-runtime",
  "take_command_id": "last-confirmed-take",
  "expected_role_map": {"on_air": "B", "preview": "A"},
  "expected_revisions": {"program": 1, "preview": 5, "role_map": 1},
  "scene_id": "source-scene",
  "scene_version": "sha256:source-address",
  "artifact_set": "sha256:admitted-artifacts",
  "writer_id": "current-editable-writer",
  "edit_seq": 12
}
```

Les identités source/artifact/writer viennent de l'inspection ; writer/seq sont
omis pour une scène sans auteur éditable. Les révisions Program et role_map et
le Take doivent égaler le dernier transfert Orion. La révision Preview peut être
plus récente que ce transfert, car sa préparation est indépendante. Une révision
Preview antérieure est refusée. Un edit intervenu depuis l'inspection refuse
la libération : demander une nouvelle inspection après règlement des écritures.

L'accusé `/scene_control/defaults/observed/release_program` recopie la requête et
ajoute `status`, `reason` éventuelle, `target`, `retired` et `replayed` éventuel.
`released` confirme le Drop et son ACK natif, le nettoyage desired/current/observed
Program et la persistance de la sélection sans Program. `pending` indique la
réconciliation en cours. `failed` distingue une erreur de livraison, d'arrêt ou
de persistance ; `retired: true` annonce que l'autorité a déjà été retirée.
Un refus périmé ne touche ni auteur ni document. Une release n'est pas un switch.

Un `attempt` entier optionnel, initialement 0/omis, peut être augmenté pour
redélivrer le même command_id après une réception absente. Seul attempt peut
changer : les autres champs sont comparés pour l'idempotence. Une release déjà
retirée attend seulement son ACK restant ; aucun nouveau Drop et aucune
libération du prochain Program ne sont exécutés. Le cache de reçus RAM est borné
à 64 commandes et ne conserve aucun document de scène inactive.

Après libération, les admissions Program démarrées avant l'arrêt sont invalidées
et les nouvelles admissions Program refusées jusqu'au prochain Take physique
confirmé. Celui-ci promeut le Preview existant vers Program sans Load/Start ni
réinstallation du document. Avec l'ancien Program vide, l'autre lane reste vide.
Les commandes runtime et les documents mutés ne sont pas persistés.

Preuves : `internal/api/program_release_native_test.go` utilise un récepteur Rust
jetable. Les tests de publication tardive, runtime/epoch/identité périmés et
d'admission concurrente vivent auprès des propriétaires API, Host et réception.
Ce banc ne qualifie ni une vraie diffusion ni le rendu CEF de l'application.
