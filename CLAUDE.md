# Orion — contexte courant

Orion est un runtime Go local lancé par l'hôte applicatif. Il admet les capsules
ZabCanvas signées, prépare les programmes Blue validés et publie leurs sorties au
récepteur LSDP natif partagé. Solar rend le LSML avec Vision. Aucun bundle Solar
n'est compilé au switch. Program, Preview, générations et sessions de test ont
leurs identités et ressources natives distinctes.

## Points d'entrée et invariants

- cmd/orion/main.go injecte les dépendances, secrets locaux, catalogue et producer.
- internal/api/public.go est le propriétaire des routes réellement enregistrées.
- internal/api/scene_intent.go admet la référence source/Blue et tient le bail de
  commit bluehost par voie jusqu'à la barrière native ; Preview reste indépendante.
- internal/lsdpreception conserve la transaction pendant une reprise transport,
  expose l'état de livraison et refuse un résultat ambigu après perte de reçu.
- internal/streamcontrol conserve les règles et intentions en LSML, puis réconcilie
  les processus possédés. Ce fichier est un snapshot durable, pas un abonnement.
- internal/providers et internal/bluehost possèdent les capacités typées,
  disponibilité et politiques Preview. Le catalogue Blue authorable est externe.

Les anciennes routes push/store, render-bundle et LSDP/1 ne sont pas enregistrées
par l'exécutable. Les modules conservés ont leurs consommateurs documentés ; les
ADR et runbooks anciens restent des traces historiques, pas la vérité des routes.
La sélection désirée dans orion/state est raccordée au catalogue signé et aux
deux présentations Solar. Le coordinator prépare, attend le renderer et conserve
l'ancien host/bridge/frame pour compensation avant finalisation. Après celle-ci,
aucun rollback distribué d'effets externes n'est promis. Seule la sélection désirée
et les sources/capsules immuables sont persistées ; les mutations live restent en RAM.
La clé admise artifact_set_digest reste distincte de l'adresse LSML scene_version.

## Lecture et validation

Lire README.md, docs/development/scene-feature-map.md et le README du propriétaire.
Pour les contrats de source/admission : docs/development/source-render-boundary.md.
Pour une capacité : docs/development/extending-orion.md. Pour les incidents natifs :
docs/runbooks/native-recovery.md. .env.template et internal/config font autorité sur
la configuration. Ne pas confondre un port ouvert avec la readiness.

```sh
go test ./...
go vet ./...
staticcheck ./...
go build -o ./bin/orion ./cmd/orion
```

Les scénarios Rust, CEF, caméra et Marker demandent un opt-in et des preuves
séparées ; leur skip ne valide pas le chemin réel. La campagne locale du 5 octobre
2026 est référencée dans docs/development/maturity.md, avec hashes et exclusions.
