# Reprise des livraisons natives

Propriétaire : internal/lsdpreception/Producer. Cycle de vie : hôte applicatif.

Consulter /api/v1/ready et host-surface.native_delivery. pending exclut la livraison
en cours et inclut les barrières ; capacity vaut 256. recovering indique une
reconnexion transitoire ; terminal demande une reprise explicite. Les compteurs
portent sur les transactions natives, pas sur les images physiques ni les effets
externes Blue.

Lors d'une coupure, Orion conserve la requête et la queue. Si une connexion établie
est perdue, il tente une seule reconnexion immédiate : une expiration d'inactivité
ne pénalise pas le premier delta de 250 ms. Si cette reprise échoue, les tentatives
suivantes sont espacées de 250 ms. Un échec de connexion initiale utilise également
ce délai. Chaque tentative est bornée à cinq secondes et rejoue exactement le
même payload/identifiant. Aucun keepalive applicatif ni message de scène artificiel
n'est ajouté. La readiness reste invalide pendant l'incident. Une
réponse vérifiée efface l'erreur transitoire et permet le drainage dans l'ordre.
L'arrêt annule la reprise. La déduplication Rust empêche une insertion déjà
committée d'être exécutée deux fois quand sa première réponse a été perdue.
Un Dial échoué ne remplace jamais le client actif : une valeur nil typée renvoyée
par le binding ne doit pas être fermée ni considérée comme une connexion établie.
Cela évite un panic lors de l'annulation ou de la reprise d'une connexion.

Un BASE_MISMATCH après une issue de transport incertaine peut indiquer une perte
de l'historique de réponses du nœud. Orion refuse NATIVE_TRANSACTION_OUTCOME_UNKNOWN,
sans nouveau rebasing/identifiant qui pourrait dupliquer l'opération. Un rejet
BASE_MISMATCH prouvé sans issue incertaine permet encore de relire et rebaser les
feuilles. Queue pleine et erreurs de contrat/protocole restent terminales.

Un timeout HTTP/barrière n'annule pas une livraison déjà admise. Observer son
état et la génération avant une nouvelle commande. Une erreur après le commit
Host n'est pas un rollback de l'ancienne instance Blue.

Le récepteur est volatile. Tant que son parent Solar est vivant, celui-ci peut
relancer un enfant aux mêmes ports et reconstruire les ressources depuis les
derniers snapshots en RAM. La readiness signale la reprise ; les abonnements
reconnectent. Cette reprise ne restaure pas l'historique de déduplication Rust :
Orion conserve donc son refus des issues de transaction ambiguës.

Après arrêt du parent et d'Orion, des processus neufs reconstruisent les scènes
depuis les sources/capsules immuables et la sélection désirée sur disque, sous
réserve d'attestations encore valides et d'un cache disponible à Solar. Les
résultats Blue et les mutations LSML live ne sont pas persistés. Il n'existe aucun
LSML de scène muté sauvegardé, outbox durable ou journal exactement-une-fois
transverse. Une reconnexion TCP ne certifie pas une reprise complète de l'app.

Preuves : recovery_test.go, idle_recovery_test.go (reprise immédiate avec identité
inchangée, backoff après échec), TestIdleSocketRecoveryNative (socket privé fermé,
récepteur vivant, application réelle) et TestEditableIdleDeltaNative (WS/auteur/ACK
après une pause sur récepteur privé, document réel optionnel).
Ces probes utilisent ORION_TEST_PRIVATE_LANE_ADDRESS d'un récepteur jetable.
Le probe WS accepte ORION_TEST_IDLE_SCENE_PATH pour un snapshot natif local,
ORION_TEST_IDLE_LEAF pour la feuille et ORION_TEST_IDLE_PAUSE_MS (maximum 180000).
Il ne publie ni valeurs de scène ni credentials et ne doit jamais cibler le dev commun.
Autres preuves : TestRealNativeTransportRecovery, tests Solar de
packaging/cycle de vie et preuve CEF complète. Garder distincts le reçu serveur,
la soumission d'image Solar et la capture physique Pulsar.
