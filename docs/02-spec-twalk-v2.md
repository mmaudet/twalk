# Twalk v2 · Spec (brouillon)

*Brouillon v0.1 · 29 octobre 2026 · à valider par Michel-Marie MAUDET*
*Contexte : voir `01-retrospective-twalk-v1.md`.*

## 1. Objectif

Te permettre de suivre sans effort ce qui se passe sur WhatsApp, Telegram, Signal, Mattermost et Matrix : **être alerté de ce qui compte, recevoir un résumé du reste, et disposer de brouillons de réponse**, sans jamais rien envoyer en ton nom.

**Critère de succès** (au bout d'un mois d'usage) : tu ne rates plus de message important, tu ne parcours plus les groupes à la main, et moins d'une alerte sur cinq est jugée inutile.

## 2. Cas d'usage

1. **Alerte** : un message urgent ou qui t'est adressé (message direct d'un proche ou d'un collaborateur, mention dans un groupe, mot-clé) arrive. Tu reçois sur Telegram, en moins de deux minutes : qui, où, quoi, pourquoi c'est important.
2. **Brouillon** : pour un message qui appelle une réponse, l'alerte contient un brouillon prêt à copier, dans ton style, avec le contexte de la conversation. C'est toi qui envoies.
3. **Résumé** : le brief matinal de 6 h 30 gagne une section « Messageries » : ce qui s'est dit d'important dans tes groupes et communautés depuis la veille, et les messages restés sans réponse.
4. **Question** : « Qu'est-ce que Gilo a dit sur BYT cette semaine ? » trouve la réponse dans l'historique collecté.
5. **Silence** : une conversation que tu as exclue (famille, par exemple) n'est jamais collectée.

## 3. Hors périmètre de la v1

Envoi de messages, multi-utilisateur, onboarding, interface web, Buzz, multilingue, Kubernetes, consentement contact par contact, mail et agenda (déjà couverts par `agent-mail` et le skill Twake), réseaux non listés.

## 4. Principes

1. **Un seul utilisateur, une seule machine** : toi, sur Athena.
2. **Lecture seule** : les collecteurs n'ont aucun code d'envoi. Hermes produit des brouillons, tu envoies toi-même.
3. **Filtrage à la source** : les exclusions s'appliquent dans le collecteur. Ce qui est exclu n'est ni publié ni stocké.
4. **Matrix est un réseau, pas un pivot** : collecteur direct par défaut, bridge mautrix seulement si le collecteur direct ne tient pas.
5. **Hermes Agent est le cerveau** : pas de runtime d'agents maison.
6. **Règles d'abord, modèle ensuite** : le tri commence par des règles déterministes, puis Jev via l'AI Gateway LINAGORA. Seuls les messages retenus remontent à Hermes.
7. **Un contrat petit** : 6 types d'événements au maximum.
8. **Petit par construction** : budgets de code et de services, mesurés chaque semaine (section 11).

## 5. Architecture

```
 WhatsApp   Telegram   Signal    Mattermost   Matrix
    │          │          │           │          │
 collecteur collecteur collecteur collecteur collecteur     (lecture seule, exclusions)
    └──────────┴──────────┴─────┬─────┴──────────┘
                                ▼
                 NATS JetStream « twalk2 » (Athena, local)
                       │                         ▲
                       ▼                         │
                 trieur (règles + Jev) ── triage.labeled
                       │
       urgent / à répondre                   info / bruit
                       ▼                         ▼
     Hermes Agent (profil « twalk »,       résumé quotidien
     plugin hermes-jetstream)              (brief de 6 h 30)
                       │
                       ▼
            Telegram : alerte + brouillon
```

| Composant | Rôle | Techno (proposée) | Budget |
|---|---|---|---|
| NATS | Bus, rétention, rejeu, permissions | `nats-server` avec JetStream, lié à `127.0.0.1` | Configuration seule |
| Collecteurs (×5) | Lire un réseau, normaliser, publier | Go, un binaire par réseau | ≤ 500 lignes chacun |
| `twalk2-event` | Types communs, validation, publication | Go, paquet partagé | ≤ 400 lignes |
| Trieur | Règles YAML, classement par Jev, étiquetage | Go ou Python | ≤ 600 lignes |
| Archiviste (lot 4) | Projection SQLite plein texte pour les questions | Go ou Python | ≤ 400 lignes |
| Hermes Agent | Alertes, brouillons, résumé, questions | Existant + plugin `hermes-jetstream` | Configuration et skill |

**Pourquoi Go pour les collecteurs** : les meilleures bibliothèques existent en Go pour WhatsApp (whatsmeow, utilisé par mautrix-whatsapp), Telegram avec un compte utilisateur (gotd), Matrix avec chiffrement de bout en bout (mautrix-go). On obtient un seul langage et des binaires statiques. Signal passe par le démon `signal-cli` (JSON-RPC) : le collecteur n'est alors qu'un adaptateur. **À trancher (section 13).**

## 6. Contrat d'événements v2

CloudEvents 1.0 en JSON, conforme au [binding NATS de CloudEvents](https://github.com/cloudevents/spec/blob/main/cloudevents/bindings/nats-protocol-binding.md). Le header `Nats-Msg-Id` reprend l'`id` de l'événement, pour que JetStream dédoublonne.

| Type (`fr.linagora.twalk.*.v2`) | Produit par | Sens |
|---|---|---|
| `message.received` | collecteur | Un message d'un tiers t'est parvenu |
| `message.sent` | collecteur | Tu as écrit toi-même, depuis n'importe quel appareil (sert à savoir si tu as répondu) |
| `reaction.added` | collecteur | Une réaction, de toi ou d'un tiers (optionnel en lot 0) |
| `collector.status` | collecteur | Connecté, déconnecté, session expirée (QR à rescanner) |
| `triage.labeled` | trieur | Priorité : `urgent`, `reply`, `info` ou `noise`, avec le motif |
| `suggestion.produced` | Hermes | Brouillon proposé, pour l'historique |

**Sujets NATS** : la structure est dans le sujet, ce qui permet de filtrer et de donner des permissions au niveau du bus (leçon v1).

```
twalk2.msg.received.<réseau>.<dm|group|channel>.<conv>
twalk2.msg.sent.<réseau>.<dm|group|channel>.<conv>
twalk2.reaction.<réseau>.<conv>
twalk2.collector.status.<réseau>
twalk2.triage.<urgent|reply|info|noise>.<réseau>.<conv>
twalk2.suggestion.<réseau>.<conv>
```

`<conv>` est un hachage court et stable de l'identifiant de conversation : le contexte d'une conversation se récupère avec un simple filtre de sujet.

**Exemple** (`message.received`) :

```json
{
  "specversion": "1.0",
  "type": "fr.linagora.twalk.message.received.v2",
  "source": "twalk2://athena/collector/whatsapp",
  "id": "whatsapp:3EB0A1C2D3",
  "time": "2026-11-03T08:12:44Z",
  "network": "whatsapp",
  "datacontenttype": "application/json",
  "data": {
    "conversation": { "id": "whatsapp:120363…@g.us", "kind": "group", "name": "Comex" },
    "sender": { "id": "whatsapp:33…", "name": "Gilles LEHOUX", "known": true },
    "text": "…",
    "reply_to": null,
    "mentions_owner": true,
    "attachments": [ { "kind": "image", "caption": "…", "ref": "obj://twalk2-media/…" } ]
  }
}
```

Les pièces jointes vont dans un Object Store JetStream à durée courte. En v1, seuls la légende et le type sont exploités.

## 7. Collecteurs

| Réseau | Accès | Mode | Points d'attention | Lot |
|---|---|---|---|---|
| WhatsApp | Ton compte, appareil lié supplémentaire | whatsmeow | Pas d'historique antérieur à la liaison. Faible risque de restriction de compte. Ne remplace pas le bridge self-chat d'Hermes | 0 |
| Telegram | Ton compte utilisateur (MTProto), `api_id` à créer par toi sur my.telegram.org | gotd | Lecture seule stricte. L'API bot ne voit pas tes conversations | 0 |
| Signal | Appareil lié via `signal-cli` en démon | Adaptateur JSON-RPC | Pas d'historique antérieur | 3 |
| Mattermost | Token personnel, WebSocket et REST | Client HTTP | Vérifier que l'instance autorise les tokens personnels | 3 |
| Matrix | @mmaudet:linagora.com, appareil dédié et vérifié | mautrix-go (E2EE) | Récupération des clés pour les salons chiffrés | 3 |

**Règles communes** :
- Aucun appel d'envoi n'est compilé dans le binaire.
- Exclusions (`exclude.yaml`) par conversation, par contact ou par réseau, appliquées avant publication.
- Les comptes techniques (bots de bridges, comptes de service) ne sont jamais traités comme des personnes.
- Un `collector.status` est publié au démarrage, toutes les 5 minutes et à chaque changement. Une session expirée déclenche une alerte Telegram.
- Configuration par fichier et variables d'environnement. Secrets dans `$HERMES_HOME/secrets/twalk2/` (droits 0600), jamais dans le dépôt.

## 8. Tri et actions

**Étape 1, règles** (`rules.yaml`, sans modèle) :
- Message direct d'un contact de la liste « proches » ou « direction » : `urgent`.
- Mention de toi, ou réponse à un de tes messages : `reply`.
- Mot-clé configuré (client, incident, signature…) : `urgent` ou `reply`.
- Groupe en sourdine : `info`, sans passer par le modèle.
- Tu as déjà répondu après ce message (`message.sent` plus récent dans la conversation) : pas d'alerte.

**Étape 2, Jev** via l'AI Gateway, uniquement pour ce que les règles n'ont pas tranché. Seuls le texte, l'expéditeur et le nom de la conversation sont transmis. Seuils réglables, comme pour `agent-mail`.

**Étape 3, actions** :

| Étiquette | Action | Où |
|---|---|---|
| `urgent` | Alerte immédiate, avec un brouillon si une réponse est attendue | Telegram (DM avec le bot) |
| `reply` | Alerte groupée toutes les 30 minutes au maximum, chacune avec son brouillon | Telegram |
| `info` | Section « Messageries » du brief de 6 h 30 | Brief matinal |
| `noise` | Rien (conservé sur le bus pour la recherche) | |

**Côté Hermes** :
- Profil dédié `twalk`, comme le recommandait l'ADR 0032 de la v1. Outils réduits : pas de terminal, aucun outil d'envoi. Lecture seule du skill Twake (agenda) pour proposer des créneaux.
- Réveil par le plugin `hermes-jetstream` sur `twalk2.triage.urgent.>` et `twalk2.triage.reply.>`. Gabarit de champs nommés : le contenu des tiers est encadré et traité comme non fiable (injection de prompt).
- **Pas d'écriture en mémoire** à partir du contenu des tiers.
- Contexte d'un brouillon : les 20 derniers messages de la conversation, récupérés par filtre de sujet.
- Format d'une alerte : `⚠️ WhatsApp · Comex · Gilles LEHOUX` / l'essentiel en une ligne / le pourquoi / le brouillon dans un bloc à copier.

## 9. Confidentialité et sécurité

- **Tout reste sur Athena** : bus, pièces jointes, archive. NATS n'écoute qu'en local.
- **Permissions NATS par composant** : chaque collecteur ne peut publier que sous `twalk2.*.<son réseau>.>`. Le trieur lit `twalk2.msg.>` et publie sous `twalk2.triage.>`. Hermes lit `twalk2.>` et ne publie que sous `twalk2.suggestion.>`.
- **Rétention** : textes et événements 30 jours, pièces jointes 7 jours, 2 Gio au maximum. Archive SQLite (lot 4) : 90 jours, réglable.
- **Modèles** : Jev (infrastructure LINAGORA) pour tout le tri. Seuls les messages `urgent` ou `reply` partent vers le modèle principal d'Hermes, avec le contexte strictement nécessaire.
- **RGPD** : collecter les messages de tiers, y compris dans un cadre professionnel, sort probablement de l'exemption « usage domestique ». **À valider avec le DPO de LINAGORA avant le lot 3.** Les exclusions par conversation et les durées courtes vont dans ce sens.

## 10. Lots

Chaque lot se termine par une démonstration sur Athena avec tes vraies données, puis **une semaine d'usage** avant le lot suivant.

| Lot | Contenu | Critère d'acceptation | Durée cible |
|---|---|---|---|
| **0 · Collecte** | NATS, `twalk2-event`, collecteurs WhatsApp et Telegram, exclusions, `collector.status` | 7 jours de collecte sans perte constatée ; volumes mesurés par réseau et par conversation ; redémarrage sans doublon | 2 à 3 jours |
| **1 · Tri et résumé** | Trieur (règles et Jev), section « Messageries » du brief | Tu valides le classement sur un échantillon de 100 messages : moins de 10 % d'erreurs gênantes | 2 jours |
| **2 · Alertes et brouillons** | Profil Hermes `twalk`, `hermes-jetstream`, alertes et brouillons Telegram | Alerte en moins de 2 minutes ; moins de 20 % d'alertes jugées inutiles sur une semaine | 2 à 3 jours |
| **3 · Réseaux** | Signal, Mattermost, Matrix | Mêmes critères que le lot 0, réseau par réseau | 1 à 2 jours par réseau |
| **4 · Mémoire** | Archiviste SQLite plein texte, questions sur l'historique | Réponse sourcée (réseau, conversation, date) à 10 questions de test | 2 jours |
| **5 · Option** | Brouillon natif dans Telegram (`messages.saveDraft`), seule exception à la lecture seule | Décision explicite de ta part après un mois d'usage | |

## 11. Garde-fous de développement

- **Budgets** : environ 4 000 lignes au total jusqu'au lot 4, tests compris ; 7 services au maximum (NATS, 5 collecteurs, trieur). Un dépassement déclenche une revue, pas un nouveau composant.
- **Contrat** : 6 types au maximum. Ajouter un type exige un cas d'usage observé en usage réel.
- **ADR** : uniquement pour une décision difficile à défaire, une page au maximum, 10 au total visés pour la v2.
- **Spec par lot** : une page, écrite avant le code, avec ses critères d'acceptation.
- **Travail avec des agents** : un lot à la fois, des PR de moins de 400 lignes de diff, relues par un humain. Aucun nouveau lot tant que le précédent n'a pas tourné une semaine.
- **Mesure hebdomadaire** : lignes par composant, nombre de services, nombre d'alertes et taux d'alertes inutiles.
- **Critère d'abandon par réseau** : si un collecteur direct dépasse 500 lignes ou casse plus d'une fois par semaine, on bascule ce réseau sur un bridge mautrix plus un mini-collecteur Matrix.

## 12. Ce qu'on réutilise

- **v1** : schémas et fixtures (simplifiés), configuration du stream, règles de normalisation (bots, messages du propriétaire), ADR 0018, 0021, 0026, 0028, 0037 et 0042.
- **Athena** : Hermes Agent et la livraison Telegram, le brief de 6 h 30, le skill Twake, l'AI Gateway et Jev (déjà utilisés par `agent-mail`).
- **Externe** : plugin [hermes-jetstream](https://hermes-agent.nousresearch.com/docs/plugins/hermes-jetstream), whatsmeow, gotd, mautrix-go, signal-cli. Idées à surveiller : le graphe d'identité multi-canal d'[Omni](https://github.com/automagik-dev/omni), utile pour reconnaître une même personne d'un réseau à l'autre.

## 13. Décisions (30 octobre 2026)

1. **Dépôt** : nouveau dépôt. La v1 sera archivée avec un lien vers la rétrospective.
2. **Exclusions** : aucune pour le moment. Le mécanisme `exclude.yaml` est quand même livré au lot 0.
3. **Telegram** : `api_id` créé (stocké dans `secrets/twalk2/telegram.env`). Michel-Marie saisit lui-même l'`api_hash` et le code de connexion.
4. **RGPD** : phase d'expérimentation, on avance. Revue DPO avant tout usage au-delà de l'expérimentation.
5. **NATS** : serveur local en place sur Athena (`127.0.0.1:4250`, stream `twalk2`, permissions par composant vérifiées).

## 14. Questions ouvertes

1. **Langage** : Go recommandé (voir section 5), décision attendue.
2. **Nom et organisation du dépôt** (`linagora/twalk2` ?), visibilité, licence AGPLv3 conservée ?
3. **Listes de priorité** : qui figure dans « proches » et « direction » ? Peut-on partir de `birthdays.json` et de l'annuaire Twake ?
4. **Mattermost** : quelle instance, et les tokens personnels y sont-ils autorisés ?
5. **Brouillons** : un exemple de 10 à 20 de tes messages par réseau, pour calibrer le style ?
