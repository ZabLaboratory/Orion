# Orion

Orion est le runtime local d'exÃ©cution broadcast. Il admet les capsules signÃ©es
ZabCanvas, exÃ©cute leurs programmes Blue validÃ©s, expose les contrats opÃ©rateur
et publie les mutations LSML au rÃ©cepteur LSDP natif partagÃ©. Solar rend ces
documents avec Vision. Un changement de scÃ¨ne ne compile aucun bundle Solar.

Program et Preview ont chacun leur instance Blue et leur ressource native.
Les stream-rules ont leurs propres instances et survivent au changement de scÃ¨ne.
Le LSML durable stream-control conserve les rÃ¨gles activÃ©es et l'intention des
applications ; le contrÃ´leur de processus maintient leur Ã©tat rÃ©el, dont Marker.

## ResponsabilitÃ©s

| ResponsabilitÃ© | PropriÃ©taire | Contrat |
|---|---|---|
| DÃ©marrage et autoritÃ© locale | cmd/orion/main.go | Loopback et secret de handshake par requÃªte |
| Admission de scÃ¨ne | internal/api/scene_intent.go | RÃ©fÃ©rence signÃ©e, digests, bail de commit par voie, barriÃ¨re native |
| ExÃ©cution Blue | internal/bluehost et internal/bluewire | Step initial avant ticks ; calls/awaits typÃ©s, Ã©vÃ©nements bornÃ©s |
| CapacitÃ©s | internal/providers et bluehost/effect_*.go | DisponibilitÃ© configurÃ©e et politique Preview explicite |
| Publication et reprise native | internal/lsdpreception | IdentitÃ© de transaction conservÃ©e, queue bornÃ©e, refus des issues ambiguÃ«s |
| RÃ¨gles et processus locaux | internal/streamcontrol | Intention atomique durable, PID possÃ©dÃ©, rÃ©conciliation et OFF |
| Affichage et mÃ©dias | Solar, Vision, Pulsar | RÃ©vision source exacte, textures camÃ©ra, composition CEF |

Dans le document de scène complet, scene_version est l'adresse source LSML ;
artifact_set_digest identifie l'ensemble admis ; Blue et l'interface opérateur
utilisent scene_digest. Le champ scene_version de desired/observed désigne la clé
du catalogue artifact_set_digest. Ces identités peuvent différer : voir le
[contrat de sélection](docs/development/scene-control.md). L'hôte applicatif lance
le nœud natif avec le serveur packagé Solar ; Orion le vérifie et l'utilise sans
second daemon.

## Surface opÃ©rateur et diagnostic

HTTP Ã©coute sur loopback 4007, les diagnostics sur 4017 par dÃ©faut.
internal/api/public.go est le propriÃ©taire canonique des routes.

| EntrÃ©e | Comportement |
|---|---|
| POST /api/v1/host/scene-intent | Admission Preview ou Take Program |
| GET /api/v1/host/status | IdentitÃ©s chargÃ©es et derniÃ¨re projection |
| GET /api/v1/cockpit/contracts | Inputs source, triggers et awaits |
| POST /api/v1/operator/call/{blueprint}/{entrypoint} | Appel opÃ©rateur admis |
| POST /api/v1/operator/resolve/{blueprint}/{await} | RÃ©solution typÃ©e |
| GET /api/v1/runtime/host-surface | CapacitÃ©s, disponibilitÃ©, limites et Ã©tat de livraison native |
| GET /api/v1/show/stream-rules | Instances globales activÃ©es |
| GET /api/v1/show/overlay-apps | Ã‰tat rÃ©el PID/running/error |
| GET /api/v1/ready | DÃ©pendance native et contrÃ´leur local vÃ©rifiÃ©s |

Les ressources de rendu sont solar/program, solar/preview, solar/generations et
solar/sessions. orion/state porte les projections de contrÃ´le. Les anciennes
routes push/store et LSDP/1 de rendu ne sont pas enregistrÃ©es par l'exÃ©cutable.
Les modules de compatibilitÃ© conservÃ©s ont des consommateurs/tests explicites.

## DÃ©veloppement et qualification

```sh
go test ./...
go vet ./...
staticcheck ./...
go build -o ./bin/orion ./cmd/orion
```

Consulter .env.template et internal/config pour les paramÃ¨tres rÃ©els : secret
local, adresse/ressource native, clÃ©s de confiance Canvas et capsules exactes.
Un port ouvert ne prouve pas la readiness. Les tests natifs/Marker sont opt-in ;
un skip ne compte jamais comme preuve d'exÃ©cution complÃ¨te.

- [Carte des fonctionnalitÃ©s](docs/development/scene-feature-map.md)
- [FrontiÃ¨re source et rendu](docs/development/source-render-boundary.md)
- [Ajouter une capacitÃ©](docs/development/extending-orion.md)
- [Reprise native](docs/runbooks/native-recovery.md)
- [Maturite locale et preuves](docs/development/maturity.md)
- [Repères Codex](docs/development/codex-project-context.md)
- [Publication native](internal/lsdpreception/README.md)
- [ContrÃ´le durable](internal/streamcontrol/README.md)

Le contrat orion.scene-control.v1 dans orion/state coordonne la sélection de scène
Blue/contrats/Solar. L'intégration d'un émetteur Prism reste hors de ce changement.
Les limites de qualification et d'intégration sont décrites dans la
[frontière source et rendu](docs/development/source-render-boundary.md) et la
[maturité locale](docs/development/maturity.md).
