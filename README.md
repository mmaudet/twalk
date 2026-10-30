# Twalk v2

Collecte en lecture seule des messageries personnelles (WhatsApp, Telegram, Signal, Mattermost, Matrix) vers NATS JetStream, tri par règles puis Jev, alertes et brouillons par Hermes Agent sur Telegram. Un seul utilisateur, une seule machine.

- Rétrospective de la v1 : [`docs/01-retrospective-twalk-v1.md`](docs/01-retrospective-twalk-v1.md)
- Spec : [`docs/02-spec-twalk-v2.md`](docs/02-spec-twalk-v2.md)

## État

Lot 0 en cours.

| Élément | État |
|---|---|
| NATS JetStream local (`127.0.0.1:4250`, stream `twalk2`, 30 jours, 2 Gio) | En place, permissions par composant vérifiées |
| Collecteur WhatsApp | À écrire |
| Collecteur Telegram | À écrire (`api_hash` et première connexion à faire par le propriétaire) |

## NATS

```
python3 deploy/nats/setup.py            # rend nats.conf et un compte par composant dans $HERMES_HOME/secrets/twalk2
cp deploy/systemd/twalk2-nats.service ~/.config/systemd/user/
systemctl --user enable --now twalk2-nats
nats -s nats://127.0.0.1:4250 --user admin --password … stream add --config deploy/nats/stream-twalk2.json
```

Chaque collecteur ne peut publier que sous `twalk2.*.<son réseau>.>` ; le trieur sous `twalk2.triage.>` ; Hermes sous `twalk2.suggestion.>`. Les secrets ne sont jamais dans le dépôt.
