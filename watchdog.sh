#!/bin/zsh
# whatsapp-actiond-go connection watchdog.
#
# The launchd KeepAlive only restarts the PROCESS on crash; it is blind to a
# live process whose WhatsApp websocket has gone dead (initial-dial failure,
# half-open socket, keepalive timeout). This checks the app-level /health
# endpoint and kickstarts the daemon when it is paired but not connected.
#
# A real logout clears the paired flag (whatsmeow deletes the session), so the
# paired+disconnected gate here never fights the intentional logged-out idle
# state. A cooldown prevents restart storms during a genuine network outage.
set -u

DIR="$HOME/Projects/whatsapp-actiond-go"
LOG="$DIR/logs/watchdog.log"
COOLDOWN_FILE="$DIR/logs/.watchdog-last-restart"
COOLDOWN_S=300   # at most one restart per 5 min
LABEL="ai.whatsapp.actiond-go"

[[ -f "$DIR/.env" ]] && { set -a; source "$DIR/.env"; set +a; }
TOK="${API_TOKEN:-}"
ts() { date '+%Y-%m-%dT%H:%M:%S%z'; }

restart() {  # $1 = reason
  local now last
  now=$(date +%s); last=0
  [[ -f "$COOLDOWN_FILE" ]] && last=$(cat "$COOLDOWN_FILE" 2>/dev/null || echo 0)
  if (( now - last < COOLDOWN_S )); then
    echo "$(ts) $1 but in cooldown ($(( now - last ))s ago), skipping" >>"$LOG"
    return
  fi
  echo "$now" >"$COOLDOWN_FILE"
  echo "$(ts) $1 -> kickstart" >>"$LOG"
  launchctl kickstart -k "gui/$(id -u)/$LABEL" >>"$LOG" 2>&1
}

h=$(curl -s --max-time 8 -H "X-API-Token: $TOK" http://127.0.0.1:8788/health 2>/dev/null)
if [[ -z "$h" ]]; then
  restart "health_unreachable"
  exit 0
fi

read paired connected <<<"$(print -r -- "$h" | python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); print(d.get("paired"), d.get("connected"))
except Exception:
    print("err err")')"

if [[ "$paired" == "True" && "$connected" == "False" ]]; then
  restart "paired+disconnected"
fi
exit 0
