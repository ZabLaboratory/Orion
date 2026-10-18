# Étendre Orion

Propriétaire : internal/providers pour le catalogue, internal/bluehost pour
l'exécution. Commencer par la carte des fonctionnalités et chercher l'adaptateur
existant. Un descripteur n'est pas une implémentation ni un nœud d'éditeur Blue.

1. Déclarer l'opération dans providers.Registry : types, bornes, erreurs,
   politique Preview. Préserver les noms existants.
2. Injecter l'implémentation dans bluehost.EffectDeps depuis cmd/orion. Réutiliser
   le résolveur de routes, les politiques egress, le pool, DB, le sink de mutation
   ou le contrôleur d'applications ; ne pas créer un second propriétaire.
3. Pour une invocation générique, compléter explicitement succès et échec dans
   effect_http/effect_local. Une opération inconnue ne doit pas rester pendante.
   Les opcodes dédiés sont annoncés par la route host-surface.
4. Vérifier Program, Preview et stream-rules selon la disponibilité. La politique
   annoncée doit correspondre au comportement, dont les écritures caméra bloquées.
5. Ajouter les tests catalogue/disponibilité, les erreurs/bornes, puis une preuve
   de l'adaptateur réel. Mettre à jour le README propriétaire et la carte.

Le contrat de mutation accepte au plus 128 add/remove/replace/test sur layout,
defaults authored et animations. Identité, pins Blue et autorité caméra restent
protégés. Chaque invocation d'animation possède une identité de commande.

Les nouveaux stores conservent source LSMLZ/assets/manifeste Blue et artefacts
d'admission signés. Le cache SceneSourceStore de Solar ne confère pas l'autorité
d'exécuter Blue : Orion vérifie toujours la référence signée et le programme.

Créer un nœud authorable demande aussi le catalogue et le typage dans Blue.
La présence d'un adaptateur Orion ne rend pas automatiquement authorables
core.effect.invoke@1 ou les opcodes internes de caméra.
