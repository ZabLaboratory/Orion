# Maturité locale — prête pour l'intégration Prism

ZabCanvas valide la scène et ses Blues. Orion charge les artefacts admis, exécute
les commandes/awaits/stream-rules et rapporte dispatch, effets et erreurs.
Solar acquiert le LSMLZ ou le LSML avec ses assets et présente Vision.
Orion ne compare aucun résultat métier attendu pour décider si une Blue est vraie.

Le prochain propriétaire de changements applicatifs est **Prism**.
Le verdict est **PASS_LOCAL_READY_FOR_PRISM_INTEGRATION** : préparation et preuves
locales, sans commit, push, merge, CI distante ni déploiement. Les continuations
Prism et Lumencast ont été comparées intégralement au baseline et sont inchangées.

[Certificat, hashes et matrice de preuves](../../evidence/local-20261005-final-certification/forge/20261005T123800Z-final-chain-certificate.json).

## Critères et preuves

| Critère | Risque vérifié | Commande / preuve |
|---|---|---|
| Admission Vision sans bundle de rendu | projection ou fermeture Blue incomplète | Canvas `scripts/prove-vision-source.py` : vraie identité ZabAuth, vraie Blue publiée via ZabGate, publication/projection/validation HTTP et SQL locales, verdict persisté ; 7 assets ; aucun verdict semé |
| Source et capsule Blue indissociables | mauvaise révision, digest, assets ou déclaration | LSML/LSMLZ servis par Canvas ; manifeste auteur + définition + fermeture validée ; vérifications Solar/Orion et reprise des mêmes capsules |
| Arrêt complet puis reprise | conservation accidentelle du LSML live ou cache incomplet | Solar `scripts/prove-cold-start-cef.py --capsule ...` : deux phases, processus neufs, 4 captures CEF 1920×1080, cache/selection inchangés, zéro lecture amont hors ligne |
| Exécution générique | couverture limitée à LEC/LCK | découverte de toutes les commandes du programme réel sur les deux lanes, reçus et erreurs ; tests génériques de tous les scopes et awaits dans Orion |
| Caméras peer | signaling, départ/retour, slot réaffecté ou libéré | `scripts/prove-peer-camera-cef.py` : serveur Meet réel, caméra physique publiée dans un Pulsar séparé, deux viewers CEF receive-only, 10 captures, arrivée/départ/retour/réaffectation/libération |
| Réception native et récupération | état périmé, transaction incertaine, duplication | 4 opt-in Go avec Rust release réel ; remplacement LSML et mutation structurelle ; reprise au même port et reçu perdu |
| Installation | scripts manquants, dépendances hoistées, binaire absent | tarball npm installé dans un dossier neuf ; postinstall exécuté ; 161 fichiers dist identiques ; adaptateur Prism existant chargé en mémoire, client Orion réel et enfant récolé |
| Concurrence et maintenance | course, référence morte, drift des assets | Solar 145 tests/18 fichiers, lint/types/reachability/pins ; Orion race 1812 tests/sous-tests/22 packages, vet/staticcheck/build ; Canvas 1402 tests, ruff complet et mypy 121 fichiers |
| Préservation | modification hors autorité | état Git, diff binaire et chaque fichier non suivi Prism/Lumencast identiques au baseline |

Les tests Canvas annoncent 85 skips et un avertissement OpenAPI GET/HEAD préexistant.
Les harnais PostgreSQL, intégrations externes et santé CI ne sont pas comptés comme
des preuves réussies ; les logs de frontière et leurs raisons sont dans le certificat.
Aucune migration de schéma n'est introduite.

## Contrats stabilisés

- La source LSML conserve son adresse `scene_version`. Le programme Blue, le
  `artifact_set_digest` et la revision signée ont leurs propres identités ;
  `x-orion-artifact-set` transporte l'ensemble admis dans le document natif.
- La validation `presentation=vision` de Canvas conserve résolution/compilation
  Blue, fermeture et digests, vérifie source et assets, puis produit une admission
  sans bundle de rendu compilé. Le défaut historique `compiled` conserve uniquement
  la compatibilité du Prism actuel. Solar utilise un seul moteur Vision.
- Toute source projetée doit déclarer l'inventaire complet des assets, polices
  comprises, par leurs chemins `assets/<sha256>.<ext>`, notamment dans
  `assets.files`. Les liens implicites hors LSML ne peuvent pas être devinés.
- Le serveur commun est fourni par `@zablab/solar/server`. Le navigateur Solar
  et Orion sont consommateurs du même processus Rust ; l'hôte applicatif le possède.
- Les opérations LSDP live, résultats Blue et observations restent en RAM.
  Seuls source/assets/déclarations/programmes immuables et sélection désirée sont
  conservés. Après perte du parent, les derniers résultats dynamiques sont abandonnés.
- Un snapshot devenu périmé pendant la préparation du rendu est relu, avec quatre
  tentatives bornées et vérifications de hash intactes ; les autres erreurs restent
  visibles. Une transaction d'issue incertaine n'est pas réémise sous une nouvelle identité.
- Une libération explicite d'un slot reste vide pendant les changements de roster.
  Les feuilles caméra appartiennent à Solar et restent hors des bindings de Vision.
- Une erreur avant finalisation restaure l'ancien host/bridge/frame exact.
  Une compensation n'annule pas un effet externe déjà émis ; un reçu de dispatch
  ne prétend pas qu'un effet asynchrone est fini.

## Ce que Prism doit intégrer

1. Projeter l'inventaire complet des assets, notamment les polices, dans le LSML
   immuable, appeler la validation Canvas en mode Vision et récupérer les capsules admises.
2. Posséder le serveur natif packagé commun : origins, readiness, propagation TCP/WS,
   reprise, logout et arrêt de l'application.
3. Synchroniser source/assets/programme/déclarations au lancement, dans un stockage
   stable par compte ; réutiliser les exports Solar et le catalogue Orion existants.
4. Obtenir et renouveler les références via le flux broker autorisé existant.
   Expiration et renouvellement restent des décisions d'intégration applicative.
5. Muter la sélection souhaitée du LSML `orion.scene-control.v1` pour Program/Preview,
   puis suivre `observed` et l'acquittement du renderer.
6. Consommer les commandes, états stream-rules/Marker et erreurs Orion ;
   distribuer les capacités de capture et les rooms viewer.
7. Prouver le cycle complet de l'application installée et sa mise à jour.

## Frontières externes

La route source du Canvas **déployé** répond encore 404 : il faut publier/déployer
le candidat Canvas avant une acquisition distante par Prism. Ce n'est pas un
changement de conception supplémentaire de Solar/Orion.

Le 404 obtenu avec le bearer opérateur sur un locator **broker-only** est le refus
attendu. Il ne prouve pas une absence d'artefacts. Le flux mTLS/delegation broker
n'a pas été exercé dans cette phase et reste à vérifier dans l'intégration Prism.
La preuve actuelle emploie une clé de signature locale au producteur Canvas,
la vraie identité et la vraie compilation Blue distantes, avec HTTP/SQL locaux.

Les références réelles gardent leur durée de cinq minutes et la readiness sa minute.
La reprise démontrée est dans cette validité ; aucune autorité Blue hors ligne
indéfinie n'est ajoutée. Les six commandes découvertes sont dispatchées hors ligne
et leurs six erreurs DB_QUERY_FAILED remontent ; aucun succès métier distant
n'est revendiqué dans cette situation.

Le cache disque immuable est qualifié entre processus. Le cache IndexedDB a ses
tests de transactions/quota/éviction, mais cette preuve à froid n'est pas une preuve
de sa persistance interprocessus. Prism devra choisir le cache commun par compte.
La caméra est qualifiée en WebRTC local ; WAN/TURN et release Linux restent hors
de ces preuves Windows. La stabilité longue durée Pulsar est acceptée par l'utilisateur.

Les contrôles de workspace sont bornés aux preuves nouvelles dans leurs dépôts
et aux continuations préservées ; aucun nettoyage d'écarts historiques globaux.
Les preuves historiques restent conservées sous leurs phases d'origine. Les
anciennes mentions « gcc absent », « debug seulement », « pas de pair réel » et
« locator opérateur 404 donc artefacts manquants » sont remplacées par les résultats
et la correction de frontière ci-dessus.

## Reproduction de la consolidation

`Solar/scripts/certify-chain.py --orion <worktree> --canvas <worktree> --pulsar <exe>
--output <préfixe canonique Solar/evidence>` lit les preuves existantes, vérifie
leurs hashes et la préservation, puis écrit une synthèse dans chacun des trois
dépôts. Il ne lance, ne publie ni ne déploie un service.
