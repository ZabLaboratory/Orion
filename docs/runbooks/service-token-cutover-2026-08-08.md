# Runbook — cutover de la famille service-token durable d'Orion (2026-08-08)

> Issue Orion #307 · ADR ZabAuth 003 Amendment 3 § A3.7 step 2 · RC 46/47/51.
> Exécuté par **Keeper**, sur décision de **@ClodoCapeo** (chaîne #302→#311).
> Identité d'exécution : `agent-keeper-quasar` (`b06508b4-8e54-4532-8848-5a64af08e5e5`).

## 1. État de départ

Le merge de #311 (retrait d'`ORION_OPERATOR_TOKEN` + `EgressTokenSource`) a
déployé à 15:37Z un `.env` d'antenne sans aucun credential : `ORION_SERVICE_TOKEN`
et `ORION_OPERATOR_TOKEN` retirés, `ORION_SERVICE_REFRESH_TOKEN` et
`ORION_ENCRYPTION_KEY` présents mais **vides**. Orion tournait donc déjà en
`service_token: degraded` — `secretbox: malformed encryption key: got 0 bytes,
want 32` — avec les appels sortants porteurs de token (`db.query` topologie A,
proxy credentials) en échec fermé. Le cutover est le correctif, pas une option.

## 2. Chemin d'appel et identité — écart assumé

`.env.agent-operator` (étage 1) est en `deny` côté outillage agent : aucun JWT
opérateur n'est obtenable, donc **pas de login réel via la gateway** comme le
prévoit § A3.7 step 2.1. Le mint a été fait depuis le VPS sur `zab-internal`
(frontière de confiance déclarée, `agents/_shared/architecture.md`) avec les
headers de l'identité d'exécution — même chemin et même caveat que la purge du
2026-08-08 (`ZabAuth/docs/runbooks/orphan-service-token-purge-2026-08-08.md` § 3
et § 3.1 : l'`actor` audité est asserté, pas authentifié). Contrôle exécuté
avant le mint : le même appel **sans** headers renvoie `401`.

La graine n'a **pas** été écrite dans l'étage-1 `.env.orion` : ce fichier est
celui du sidecar embedded-local (Prism), et `.env.template` interdit
explicitement d'y placer la graine d'antenne — un sidecar qui la résoudrait
ferait tourner la famille de l'antenne en `reuse`. Le seul porteur de la graine
est le secret GitHub, consommé par l'étape « Write remote .env » du deploy.

## 3. Séquence exécutée

| Heure (UTC) | Geste | Résultat |
|---|---|---|
| 15:42:14 | mint famille **f1dfd0ae-c270-49b8-991e-f45186942714** (`service=orion`, paths `quasar.credentials.read,query.read.truth,query.read.ranking`, `bootstrap_ttl_s=604800`) | gen 0, `family_expires_at` 2027-08-08 |
| 15:42:4x | `gh secret set ORION_ENCRYPTION_KEY` (`openssl rand -base64 32`, 32 octets bruts) + `ORION_SERVICE_REFRESH_TOKEN` | posés |
| 15:46 | merge PR #312 (preflight + purge `.env.template`) → deploy | vert |
| 15:47:38 | **adoption : génération 1 émise** (`650916da-4838-4f9f-9ff4-087d965c28de`) | famille adoptée |
| 15:47:39 | second boot du conteneur → `degraded` | **incident, § 4** |

## 4. Incident — le deploy démarrait Orion deux fois

`deploy.yml` faisait `docker compose up -d` **puis** `up -d --force-recreate
orion`. Le premier conteneur bootait, prenait le lock d'avance de rotation,
posait le marqueur `rotating`, obtenait la génération 1 de ZabAuth… et était
détruit ~1 s plus tard par le recreate, **à l'intérieur exact de la fenêtre**
que le persist-before-swap encadre. Le successeur n'a existé qu'en mémoire du
conteneur mort. Le boot suivant a trouvé le marqueur et **a refusé de rejouer
une génération consommée** — comportement correct (§ 5 R8 : rejouer aurait
révoqué la famille), mais Orion reste sans token jusqu'à re-seed humain.

Ce n'était pas un aléa : le double démarrage rendait l'échec **systématique à
chaque deploy**. Correctif dans le même chantier — un seul démarrage par
deploy (`up -d orion-db` puis `up -d --force-recreate orion`).

Reste un résiduel assumé, inhérent au modèle : un recreate qui tomberait
pendant une rotation horaire produit le même état. Signature à reconnaître :

```
durable service token: the persisted credential is marked rotating …
```

## 5. Reprise après marqueur `rotating`

1. Vérifier la signature ci-dessus dans `docker logs orion` et
   `/ready` → `service_token: degraded`.
2. Purger la ligne singleton empoisonnée — elle est inutilisable **par
   construction** (le code refusera toujours de la rejouer), il n'y a donc rien
   à sauvegarder ; un dump préalable est néanmoins conservé :
   ```bash
   ssh vps-ovh "docker exec orion-postgres psql -U orion -d orion -c \
     'DELETE FROM service_token_state;'"
   ```
   *Rollback* : aucun n'est utile — restaurer la ligne remettrait le marqueur,
   donc l'état dégradé. La reprise passe obligatoirement par un nouveau mint.
3. Re-minter une famille (§ 3, ligne 1), poser `ORION_SERVICE_REFRESH_TOKEN`,
   redéployer (`workflow_dispatch` suffit).
4. Vérifier `/ready` → `armed` **et** `generation = 1` sur la nouvelle famille.
5. Révoquer la famille orpheline précédente (sa tête vivante n'est détenue par
   personne) :
   `POST /api/v1/service-tokens/{family_id}/revoke` (opérateur).
6. Purger le secret GitHub `ORION_SERVICE_REFRESH_TOKEN` — la graine est une
   génération dépensée dès la première rotation. `ORION_ENCRYPTION_KEY` ne se
   purge **jamais** : elle déchiffre la chaîne à chaque boot.

## 6. Preuves à consigner (RC 46)

- les deux `family_id` (ancienne, nouvelle) ;
- `generation = 1` observée sur la nouvelle **avant** toute révocation ;
- `/ready` en `armed` ;
- l'ancienne famille sans tête vivante après révocation
  (`select count(*) from service_tokens where family_id=… and not revoked` → 0).

Répertoire de travail VPS (`chmod 700`, contient la réponse de mint) :
`/home/ubuntu/keeper-orion-cutover-20260808/`. À supprimer une fois la famille
adoptée et les preuves consignées.
