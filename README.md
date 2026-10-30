# Twalk v2

Collecte en lecture seule des messageries personnelles (WhatsApp, Telegram, Signal, Mattermost, Matrix) vers NATS JetStream, tri par règles puis Jev, alertes et brouillons par Hermes Agent sur Telegram. Un seul utilisateur, une seule machine.

- Rétrospective de la v1 : [`docs/01-retrospective-twalk-v1.md`](docs/01-retrospective-twalk-v1.md)
- Spec : [`docs/02-spec-twalk-v2.md`](docs/02-spec-twalk-v2.md)

## État

Lot 0 en cours.

| Élément | État |
|---|---|
| NATS JetStream local (`127.0.0.1:4250`, stream `twalk2`, 30 jours, 2 Gio) | En place, permissions par composant vérifiées |
| Collecteur WhatsApp (`cmd/collector-whatsapp`, whatsmeow) | Écrit et testé hors ligne ; appairage à faire |
| Collecteur Telegram (`cmd/collector-telegram`, gotd) | Écrit et testé hors ligne ; login à faire |

## Premier lancement (par le propriétaire, dans un terminal sur Athena)

```
sudo -u hermes /home/hermes/work/twalk2/bin/collector-whatsapp pair --phone 33660469852
sudo -u hermes /home/hermes/work/twalk2/bin/collector-telegram login
```

Puis : `systemctl --user enable --now twalk2-collector@whatsapp twalk2-collector@telegram` (utilisateur hermes).

## NATS

```
python3 deploy/nats/setup.py            # rend nats.conf et un compte par composant dans $HERMES_HOME/secrets/twalk2
cp deploy/systemd/twalk2-nats.service ~/.config/systemd/user/
systemctl --user enable --now twalk2-nats
nats -s nats://127.0.0.1:4250 --user admin --password … stream add --config deploy/nats/stream-twalk2.json
```

Chaque collecteur ne peut publier que sous `twalk2.*.<son réseau>.>` ; le trieur sous `twalk2.triage.>` ; Hermes sous `twalk2.suggestion.>`. Les secrets ne sont jamais dans le dépôt.
