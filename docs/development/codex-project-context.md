# Repères Orion pour une mission Codex

Ce guide aide à cadrer une mission dans Orion. Il complète le mandat et les
instructions applicables au dépôt ; il n'installe ni agent, ni plugin, ni service.
Les sources ci-dessous ont été relues le 25 octobre 2026 sur la base
`4082a49d64d4d11d9f40c4cdcb19e141e2d77365`. Vérifier leur version avant une nouvelle mission.

## Responsabilités et sources

Orion est le runtime local qui admet les capsules signées Canvas, prépare et
exécute leurs programmes Blue, expose les contrats opérateur et publie les
mutations LSML au récepteur LSDP natif. Solar rend ces documents avec Vision.
Un changement de scène ne compile aucun bundle Solar. Les frontières détaillées
sont dans [source et rendu](source-render-boundary.md).

| Sujet | Source à consulter |
|---|---|
| Démarrage et autorité locale | [cmd/orion/main.go](../../cmd/orion/main.go) |
| Routes HTTP effectivement enregistrées | [internal/api/public.go](../../internal/api/public.go) |
| Admission, publication native et identités | [Frontière source et rendu](source-render-boundary.md) |
| Intention de scène et coordination | [Scene control](scene-control.md) et [scene_control.go](../../internal/api/scene_control.go) |
| Exécution Blue | [bluehost](../../internal/bluehost/README.md) |
| Publication et reprise native | [lsdpreception](../../internal/lsdpreception/README.md) |
| Intention durable et processus locaux | [streamcontrol](../../internal/streamcontrol/README.md) |
| Extension d'une capacité | [Guide](extending-orion.md) et [carte des fonctionnalités](scene-feature-map.md) |
| Preuves et limites | [Maturité](maturity.md) et [reprise native](../runbooks/native-recovery.md) |

Le [module Go](../../go.mod) déclare Go `1.26.2` et les dépendances du produit,
dont le runtime Blue. Les agents, outils, workflows et plugins Codex relèvent
de l'environnement de travail. Ce guide ne modifie pas ces dépendances.

## Frontières à conserver

- Dans le document de scène complet, `scene_version` adresse la source LSML ;
  `artifact_set_digest` identifie l'ensemble admis et `scene_digest` la scène Blue.
  Dans `desired/observed`, le champ `scene_version` désigne en revanche la clé
  `artifact_set_digest` du catalogue. Ne pas substituer ces identités ; suivre le
  [contrat de sélection](scene-control.md) et le [contrat source](source-render-boundary.md).
- Program et Preview ont leurs propres instances et ressources. Une preuve sur
  une voie ne valide pas automatiquement l'autre.
- Le contrat `orion.scene-control.v1` porté par `orion/state` commande l'intention
  de scène. Modifier le fichier durable `stream-control.lsml` ne constitue pas
  une commande live ; consulter [scene control](scene-control.md).
- Après la perte d'un reçu natif, conserver l'identité de transaction et suivre
  la [reprise native](../runbooks/native-recovery.md), sans réinsertion aveugle.
- Un port ouvert, la readiness, un ACK natif et l'affichage physique de Program
  sont des observations différentes. Indiquer précisément celle qui a été obtenue.

## Préparer et réaliser une mission

1. Relever la branche, le commit, les modifications et les worktrees existants.
   Consigner le périmètre autorisé, le résultat attendu et les critères d'acceptation.
2. Choisir une base explicite et un worktree dédié selon les conventions du dépôt.
   Préserver les modifications et worktrees des autres missions.
3. Localiser le propriétaire du contrat concerné dans les sources ci-dessus.
   Préciser les voies, les identités et les effets couverts par la modification.
4. Réaliser la modification, puis conserver les résultats des vérifications avec
   la révision exacte examinée.

Les rôles du socle Codex peuvent être répartis entre un **responsable** qui cadre
et décide, un **réalisateur** qui produit la modification et un **examinateur**
qui vérifie une version précise. Une tâche simple peut être menée directement.
Lorsque le mandat demande un examen indépendant, confier celui-ci à un autre
agent et lui fournir les critères, le diff et les sources, sans présumer son verdict.

## Choisir les vérifications

Les commandes suivantes figurent dans les instructions ou la politique du dépôt.
Leur présence ne constitue pas une preuve d'exécution : noter les commandes
réellement lancées, leurs résultats et les prérequis manquants.

| Commande | Usage |
|---|---|
| `go test ./...` | Suite Go |
| `go vet ./...` | Analyse Go |
| `staticcheck ./...` | Analyse statique complémentaire |
| `go build -o ./bin/orion ./cmd/orion` | Build de l'exécutable |
| `python scripts/check_file_sizes.py` | [Garde de taille des fichiers](file-size-policy.md) |

L'accès aux modules Go privés et les outils nécessaires doivent être disponibles.
Les preuves natives, CEF, caméra ou Marker ont leurs propres prérequis et scénarios ;
un test ignoré ne valide pas ces chemins. Consulter les limites de
[maturité](maturity.md) avant d'annoncer une validation de bout en bout.

Pour une modification documentaire, vérifier les sources, les liens relatifs,
la lisibilité, le diff et la garde de taille. Rapporter séparément les contrôles
locaux et la [CI](../../.github/workflows/ci.yml) ; ne pas annoncer une validation
du runtime ou du rendu à partir de la seule documentation.

## Examiner et livrer

L'examen identifie la révision ou les empreintes des fichiers, les constats et les
limites. Toute correction après examen doit être vérifiée à nouveau. La PR décrit
le changement, sa raison et les validations réellement obtenues.

Pour une livraison demandée en brouillon, vérifier après création la branche de
base, la branche source, le commit publié et l'état **Draft**. La fusion, la release
et le déploiement sont des étapes distinctes qui nécessitent leur propre mandat.
