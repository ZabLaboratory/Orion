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

L'adresse source est artifact_set_digest ; Blue et l'interface opÃ©rateur utilisent
scene_digest. Ces valeurs peuvent diffÃ©rer. L'hÃ´te applicatif lance le nÅ“ud natif
avec le serveur packagÃ© Solar ; Orion le vÃ©rifie et l'utilise sans second daemon.

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
- [Publication native](internal/lsdpreception/README.md)
- [ContrÃ´le durable](internal/streamcontrol/README.md)

Cette migration est qualifiÃ©e localement, sans publication ni dÃ©ploiement.
Un petit LSML dÃ©clarant une scÃ¨ne dÃ©sirÃ©e ne dÃ©clenche pas encore l'admission
coordonnÃ©e Blue/contrats/Solar. La synchronisation et le transfert d'autoritÃ© Prism
restent hors de ce pÃ©rimÃ¨tre. Les limites courantes sont dans la frontiÃ¨re source.
