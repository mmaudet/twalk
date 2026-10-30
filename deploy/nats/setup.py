#!/usr/bin/env python3
"""Rend la configuration NATS de Twalk v2 et génère un mot de passe par composant.

Usage : python3 deploy/nats/setup.py [--rotate]
Écrit dans $TWALK2_SECRETS (défaut : $HERMES_HOME/secrets/twalk2) :
  nats.conf, users.conf (0600) et <composant>.env (0600, NATS_URL/NATS_USER/NATS_PASSWORD).
Sans --rotate, les mots de passe existants sont conservés.
"""
import os
import secrets
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
HERMES_HOME = Path(os.environ.get("HERMES_HOME", Path.home() / "hermes-new"))
SECRETS = Path(os.environ.get("TWALK2_SECRETS", HERMES_HOME / "secrets" / "twalk2"))
STORE = Path(os.environ.get("TWALK2_STORE", Path.home() / ".local/share/twalk2/jetstream"))
PORT, MONITOR_PORT = 4250, 8250

NETWORKS = ["whatsapp", "telegram", "signal", "mattermost", "matrix"]
# Lecture d'un consommateur JetStream : API consommateur, info du stream, acks.
CONSUME = ["$JS.API.INFO", "$JS.API.STREAM.INFO.twalk2", "$JS.API.CONSUMER.>", "$JS.ACK.twalk2.>", "$JS.API.DIRECT.GET.twalk2.>", "$JS.API.STREAM.MSG.GET.twalk2"]


def collector(net):
    return {
        "publish": [f"twalk2.msg.*.{net}.>", f"twalk2.reaction.{net}.>", f"twalk2.collector.status.{net}"],
        "subscribe": ["_INBOX.>"],
    }


USERS = {f"collector-{n}": collector(n) for n in NETWORKS}
USERS["trieur"] = {"publish": ["twalk2.triage.>"] + CONSUME, "subscribe": ["twalk2.>", "_INBOX.>"]}
USERS["hermes"] = {"publish": ["twalk2.suggestion.>"] + CONSUME, "subscribe": ["twalk2.>", "_INBOX.>"]}
USERS["admin"] = {"publish": [">"], "subscribe": [">"]}


def q(items):
    return "[" + ", ".join(f'"{s}"' for s in items) + "]"


def read_env(path):
    env = {}
    if path.exists():
        for line in path.read_text().splitlines():
            if "=" in line:
                k, v = line.split("=", 1)
                env[k] = v
    return env


def write_private(path, text):
    path.write_text(text)
    path.chmod(0o600)


def main():
    rotate = "--rotate" in sys.argv
    SECRETS.mkdir(parents=True, exist_ok=True)
    SECRETS.chmod(0o700)
    STORE.mkdir(parents=True, exist_ok=True)
    url = f"nats://127.0.0.1:{PORT}"
    blocks = []
    for name, perm in USERS.items():
        env_path = SECRETS / f"{name}.env"
        pw = read_env(env_path).get("NATS_PASSWORD")
        if rotate or not pw:
            pw = secrets.token_urlsafe(32)
        write_private(env_path, f"NATS_URL={url}\nNATS_USER={name}\nNATS_PASSWORD={pw}\n")
        blocks.append(
            f'    {{ user: "{name}", password: "{pw}", permissions: {{ '
            f'publish: {q(perm["publish"])}, subscribe: {q(perm["subscribe"])} }} }}'
        )
    write_private(SECRETS / "users.conf", "authorization {\n  users: [\n" + ",\n".join(blocks) + "\n  ]\n}\n")
    tmpl = (HERE / "nats.conf.tmpl").read_text()
    write_private(SECRETS / "nats.conf", tmpl.format(port=PORT, monitor_port=MONITOR_PORT, store_dir=STORE))
    print(f"OK : {SECRETS}/nats.conf, {len(USERS)} comptes, store {STORE}, {url}")


if __name__ == "__main__":
    main()
