# Rétrospective Twalk v1

*Brouillon · 29 octobre 2026 · rédigé par Hermes pour Michel-Marie MAUDET*

## En bref

Twalk v1 part d'une bonne idée : un flux unique d'événements typés (CloudEvents sur NATS JetStream) alimenté par toutes les messageries personnelles, sur lequel des agents raisonnent sous contrôle humain. L'exécution a dérivé vers une plateforme multi-utilisateurs, avec onboarding, consentement contact par contact, approbation, envoi et quatre surfaces d'interface, avant que le chemin le plus simple (collecter, trier, alerter, proposer un brouillon) n'ait servi un seul jour. Le résultat : environ 244 000 lignes écrites en 19 jours, 42 ADR, 19 services Docker, puis l'arrêt du projet le 5 octobre.

**Recommandation : garder le contrat et les leçons, réécrire une v2 courte, à un seul utilisateur, en lecture seule, branchée directement sur Hermes Agent.**

## Méthode et limites

- Analyse statique de la branche `main` de [linagora/twalk](https://github.com/linagora/twalk), commit `d185923` du 5 octobre 2026 : code, ADR, README, feuille de route, historique git, compteurs GitHub.
- Je n'ai **pas** exécuté la pile, et je n'ai pas eu accès à un éventuel déploiement sur Athena (dossier `/home/mmaudet` fermé à Hermes).
- Le dépôt ne montre aucune trace d'usage réel (métriques d'exploitation, retours d'usage). **À confirmer par toi.**

## Chiffres clés

| Indicateur | Valeur |
|---|---|
| Période d'écriture | 17 septembre au 5 octobre 2026 (19 jours) |
| Commits | 677, dont 253 merges de PR ; **475 commits les 4 premiers jours** (96, 122, 103, 154 par jour) |
| Co-signature Claude | 396 commits |
| Tickets | 213 fermés, 27 ouverts ; 151 branches distantes |
| Volume | ~244 000 lignes : Rust 150 000 (dont ~61 000 de tests), TS et Svelte 53 000, JSON/YAML 24 000, Python 7 300 |
| Décisions | 42 ADR (0005 à 0042), rédigées en prose dense |
| Contrat | 17 schémas d'événements (le README en annonce 9, la feuille de route 10) |
| Exploitation | 19 services Docker Compose (dont 8 pour 4 bridges mautrix, derrière un profil) |
| Dépendances Rust directes | Gateway 57, clerk 43, collecteur 36, Sensor 21, runtime de personas 21 |

## Architecture effective

```
WhatsApp, Signal, SMS, Telegram
        │  (bridges mautrix, appservices)
        ▼
Synapse ── salons portails chiffrés ──► Sensor (déchiffre, enrichit, filtre consentement)
                                            │
                                            ▼
                                   NATS JetStream « twalk »
             ┌──────────────┬───────────────┼────────────────┬───────────────┐
             ▼              ▼               ▼                ▼               ▼
   Runtime personas   Companion Gateway   Clerk          Collecteur      (Hermes Agent
   (Rust, Docker      (consentement,     (projection     mail/agenda     via webhook,
   socket) + persona  approbation,       vers Buzz,      JMAP/CalDAV)    ADR 0032)
   Python + SDK       provisioning)      relais Nostr)
                            │
                            ▼
                     Companion (PWA SvelteKit)
```

| Composant | Rôle | Lignes (src + tests) | Constat |
|---|---|---|---|
| `companion-gateway` | Consentement, auth, approbation, provisioning des bridges, réglages, contacts | 70 400 Rust | **Le plus gros composant**, alors qu'il ne collecte ni ne raisonne |
| `companion` | PWA d'onboarding et de réglages | 52 700 TS/Svelte | Pensé pour un utilisateur non technique |
| `sensor` | Client Matrix, déchiffrement, normalisation, publication | 24 400 Rust | Le vrai cœur, trois fois plus petit que le Gateway |
| `clerk` | Projection du bus vers Buzz (Nostr), approbations par réaction ✅ | 18 900 Rust | Une surface d'interface de plus, avec sa propre clé et ses propres boucles |
| `collector` | Mail et agenda (OIDC, JMAP, CalDAV), règles de tri du mail | 17 200 Rust | Doublon d'`agent-mail` et du skill Twake qui tournent déjà |
| `hermes` | Runtime de personas en Rust (supervision de conteneurs) | 11 200 Rust | Réimplémente une partie de ce que fait Hermes Agent |
| `sdk` | SDK Python des personas (barrière de consentement, enveloppes) | 5 900 Python | |
| `contracts` | Schémas CloudEvents, fixtures, phrases de transparence | 3 300 JSON | **Bon actif, à garder** |
| `consent-cache` | Cache partagé du consentement | 1 600 Rust | Existe uniquement à cause du modèle de consentement |

## Ce qui est bon, et qu'on garde

1. **L'idée centrale** : un bus d'événements typés, rejouable, découplé des réseaux et des agents.
2. **CloudEvents 1.0 sur NATS JetStream** : standard, léger, avec rejeu, consommateurs durables et dédoublonnage par `Nats-Msg-Id`.
3. **Les messages envoyés par toi ont leur propre type d'événement** ([ADR 0018](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0018-the-users-own-messages-are-their-own-event-type.md), [ADR 0021](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0021-the-owner-is-never-a-contact-on-any-event.md)). Indispensable pour savoir si tu as déjà répondu.
4. **Un bot de bridge n'est pas une personne** ([ADR 0026](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0026-a-bridge-bot-is-not-a-person.md)) : filtrer les comptes techniques dès la collecte.
5. **Rétention bornée sur le bus** ([ADR 0037](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0037-the-bus-keeps-ninety-days-and-two-gigabytes-and-no-more.md)) : durée et taille plafonnées, blocs compressés.
6. **Le texte des tiers vit moins longtemps que les métadonnées** ([ADR 0028](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0028-a-contacts-words-live-apart-from-the-event-that-identifies-them.md)). On garde le principe, en plus simple.
7. **Le tri suit tes règles avant le jugement d'un modèle** ([ADR 0042](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0042-mail-triage-is-the-owners-rules-and-not-the-agents-judgement.md)) : règles déterministes d'abord, modèle ensuite.
8. **Pas de modèle par défaut** ([ADR 0015](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0015-no-default-llm-configured-through-the-companion.md)) : rien n'est envoyé à un modèle que tu n'as pas désigné.
9. **Le contenu d'un message ne va jamais brut dans la mémoire de l'agent** ([ADR 0032](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0032-twalk-governs-what-an-agent-outside-it-may-see-and-do.md)) : gabarit de champs nommés, profil Hermes séparé.
10. **Des fixtures validées pour chaque schéma** : la discipline de test du contrat est bonne.

## Ce qui n'a pas marché

### 1. Une cible produit qui n'est pas la tienne
Twalk v1 vise un « utilisateur non technique » qui part de zéro : création de compte Matrix, connexion des bridges, activation des personas, cinq langues d'interface, PWA mobile. Le seul utilisateur réel, c'est toi, sur une machine que tu administres. Toute la chaîne Companion + Gateway (environ 123 000 lignes) sert ce cas qui n'existe pas encore.

### 2. La complexité est loin du cœur
Le Sensor, qui collecte et normalise, fait 24 000 lignes. Le Gateway, qui ne collecte ni ne raisonne, en fait 70 000. Les plus gros fichiers du Gateway parlent de stockage du consentement, d'approbations, de bridges, de réponses d'Hermes et de disponibilités d'agenda : autant de fonctions périphériques.

### 3. Matrix obligatoire, avec un coût caché
Passer tout le trafic par Synapse impose une série de mécanismes qui n'existent qu'à cause de Matrix :
- l'invitation du Sensor dans chaque salon portail, une conversation à la fois ([0024](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0024-a-portal-room-is-observed-by-invitation-per-conversation.md)) ;
- le chaînage des salons remplacés ([0029](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0029-a-conversation-outlives-its-room-but-the-room-stays-the-key.md)) ;
- un appareil Matrix de l'utilisateur pour agir en son nom, créé dans le navigateur, transmis chiffré au Sensor, avec son propre événement d'état ([0025](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0025-twalk-acts-as-the-user-through-a-device-of-their-account.md), [0034](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0034-the-users-device-credential-goes-from-browser-to-sensor-and-is-never-relayed.md), [0041](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0041-the-owner-device-says-on-the-bus-whether-it-can-act.md)) ;
- les clés de signature croisée générées dans le navigateur ([0014](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0014-companion-crypto-runs-in-the-browser.md)).

Même Mattermost, qui a une API WebSocket directe, était prévu via un connecteur mautrix à écrire (ticket #241).

### 4. Un modèle de consentement contact par contact, trop lourd pour un seul utilisateur
Instantané puis deltas ([0010](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0010-consent-snapshot-then-deltas.md)), publication réduite en cas de révocation ([0012](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0012-revoked-consent-reduces-publication.md)), activation des personas comme décision de consentement ([0013](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0013-persona-activation-is-a-consent-decision.md)), messages « en attente » rejoués à l'octroi ([0040](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0040-a-grant-reaches-the-messages-still-waiting-for-it.md)), plus un cache dédié. La rigueur est réelle, mais pour toi, une liste d'exclusion par conversation appliquée **avant** publication donne une garantie plus forte, et pour beaucoup moins cher : ce qui n'est pas publié ne peut être vu par personne.

### 5. Le chemin d'envoi construit trop tôt
L'API d'approbation, la phrase de transparence envoyée au contact ([0019](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0019-a-persona-discloses-itself-to-the-contact.md), [0031](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0031-the-disclosure-is-written-by-neither-the-model-nor-the-gateway.md)), l'approbation depuis Buzz par un appareil du propriétaire ([0036](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0036-the-clerk-approves-as-a-device-of-the-owner.md)), l'envoi de mail par JMAP ([0038](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0038-an-oidc-grant-is-the-users-identity-and-a-jmap-submission-their-device.md)) : l'envoi en ton nom est la partie la plus risquée, et elle a été construite avant d'avoir validé l'utilité des brouillons.

### 6. Quatre surfaces d'interface
La PWA Companion, Element, Buzz via le clerk (relais Nostr, trois canaux, cinq boucles), et Messagr prévu ensuite. Chaque surface a apporté ses propres règles, clés et ADR. Tu lis déjà Hermes sur Telegram.

### 7. Hermes réinventé, puis gardé à côté du vrai
Le dossier `hermes/` est un runtime de personas en Rust qui lance des conteneurs via le socket Docker de l'hôte ([0023](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0023-the-deployment-starts-personas-through-the-hosts-docker-socket.md)), avec son SDK Python. L'[ADR 0032](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0032-twalk-governs-what-an-agent-outside-it-may-see-and-do.md) découvre ensuite qu'Hermes Agent existe, qu'il sait déjà parler aux réseaux, et décide de… garder le runtime comme « frontière d'application » et d'ajouter un pont webhook et un retour par le Gateway. **La réconciliation a ajouté une couche au lieu d'en retirer une.**

### 8. Un contrat qui gonfle et un bus qui ne protège rien
- 9, puis 10, puis 17 types d'événements, et une documentation qui ne suit plus.
- Le sujet NATS ne porte pas le réseau : le README du runtime reconnaît que le filtrage par réseau « n'est pas appliqué au niveau du bus », donc une persona autorisée sur WhatsApp seul reçoit tout.
- L'ADR 0032 constate que « le bus n'a pas d'authentification ». D'où des allers-retours HTTP par le Gateway pour contourner, alors que NATS sait gérer comptes et permissions par sujet.

### 9. Des doublons avec l'existant
Le collecteur mail et agenda refait ce que font `agent-mail` (service actif sur Athena : tri Jev, rangement, brouillons) et le skill `twake-workplace`. Les règles de tri du mail ([0042](https://github.com/linagora/twalk/blob/main/docs/architecture/adr/0042-mail-triage-is-the-owners-rules-and-not-the-agents-judgement.md)) recoupent le lot 2 d'agent-mail.

### 10. Le processus : la vitesse a pris le pas sur la validation
- 475 commits en quatre jours, 42 ADR en 19 jours, des revues « le jour même ».
- Les ADR sont des essais argumentés plutôt que des décisions courtes : difficile à relire, à contester ou à maintenir seul.
- La feuille de route n'a pas été revue après le premier jour, `ui/` et `examples/` sont vides.
- Aucune boucle « je l'utilise une semaine, je mesure, je corrige » n'apparaît avant d'élargir le périmètre.

Avec un agent capable de produire 150 commits par jour, **la contrainte n'est plus l'écriture du code mais le périmètre**. Sans garde-fou explicite, chaque question ouverte devient un ADR, puis un composant.

## Leçons pour la v2

1. **Concevoir pour l'utilisateur réel** : toi, sur Athena. Le multi-utilisateur viendra après un usage prouvé, si jamais il vient.
2. **Livrer d'abord la valeur en lecture seule** : collecter, trier, alerter, proposer un brouillon. L'envoi assisté ne vient qu'après un mois d'usage, et sur décision explicite.
3. **Filtrer à la source** : ce qui est exclu n'est jamais publié. C'est plus simple et plus sûr qu'une barrière de consentement en aval.
4. **Ne pas imposer de pivot technique** : Matrix est un réseau parmi d'autres, et mautrix une option par réseau, pas un passage obligé.
5. **Ne pas réécrire ce qui existe** : Hermes Agent est le cerveau, `agent-mail` et Twake couvrent mail et agenda, Telegram est l'interface.
6. **Mettre la structure dans les sujets NATS** (réseau, type de conversation, conversation) et sécuriser le bus par des permissions NATS dès le premier jour.
7. **Un contrat petit et stable** : 6 types au maximum jusqu'à nouvel ordre.
8. **Des budgets explicites** : lignes par composant, nombre de services, nombre d'ADR. Les mesurer chaque semaine.
9. **Un ADR = une décision irréversible, une page.** Le reste va dans la spec du lot ou dans un commentaire de code.
10. **Une semaine d'usage réel entre deux lots**, avec des critères d'acceptation mesurables.

## Ce qu'on récupère concrètement

| Élément v1 | Usage en v2 |
|---|---|
| Schémas `inbound.message.received`, `outbound.message.sent`, `inbound.reaction.added`, `bridge.status.changed` | Point de départ du contrat v2, simplifié (voir spec) |
| Fixtures et tests de conformité des schémas | Repris tels quels pour le contrat v2 |
| Configuration du stream (rétention, compression, fenêtre de dédoublonnage) | Reprise, avec des durées plus courtes |
| Connaissance des bridges mautrix (configs, enregistrements) | Option par réseau si un collecteur direct s'avère trop fragile |
| Règles de normalisation du Sensor (ghosts, bots, messages du propriétaire) | Logique reprise dans les collecteurs, sans Matrix |
| ADR 0018, 0021, 0026, 0028, 0032 (partie mémoire), 0037, 0042 | Principes intégrés à la spec v2 |

## Ce qu'on abandonne

Companion (PWA), Companion Gateway, clerk et Buzz, consent-cache, runtime de personas Rust et SDK, collecteur mail et agenda, Synapse obligatoire, chemin d'envoi, multilingue, onboarding, Kubernetes.

## Sort du dépôt v1

Proposition : archiver `linagora/twalk` en lecture seule, avec en tête du README un lien vers cette rétrospective, et démarrer la v2 dans un dépôt neuf (ou une branche orpheline) pour ne pas hériter des 151 branches ni de la documentation v1.
