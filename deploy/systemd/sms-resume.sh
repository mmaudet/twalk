#!/bin/sh
# Reprise nocturne du collecteur SMS : si Messages Web a été repris par un navigateur
# (statut disconnected « session Messages Web … »), relancer le service, qui reprend
# la session. Ne fait rien si connecté, ni si logged_out (il faut réappairer).
# --dry-run : affiche la décision sans agir.
set -eu
cd "$HOME/work/twalk2"
set -a; . "$HOME/hermes-new/secrets/twalk2/hermes.env"; set +a
decision=$(bin/trieur feed --since 1m --limit 10 | python3 -c '
import sys, json
c = {x["network"]: x for x in json.load(sys.stdin)["collectors"]}.get("sms")
if not c: print("absent")
elif c["state"] == "disconnected" and "session Messages Web" in c.get("detail", ""): print("resume")
else: print("skip:" + c["state"])')
echo "sms-resume: $decision"
[ "$decision" = resume ] && [ "${1:-}" != --dry-run ] && systemctl --user restart twalk2-collector@sms && echo "sms-resume: collecteur relancé"
exit 0
