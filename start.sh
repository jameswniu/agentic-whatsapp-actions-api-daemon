#!/bin/zsh
# Launch wrapper for whatsapp-actiond-go. Loads .env and execs the daemon.
set -eu
cd "$(dirname "$0")"
if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi
exec ./actiond
