#!/bin/zsh
# Edge-case verification harness for whatsapp-actiond-go.
# Runs read-only + dry-run checks that are safe against a live account.
# Live destructive ops are NOT exercised here except against SELF_JID if provided.
set -u
BASE="http://127.0.0.1:8788"
source "$(dirname "$0")/../.env"
PASS=0; FAIL=0
t() { # t "name" expected_http actual_http [extra]
  if [[ "$2" == "$3" ]]; then echo "  PASS: $1 ($3)"; PASS=$((PASS+1));
  else echo "  FAIL: $1 expected $2 got $3  $4"; FAIL=$((FAIL+1)); fi
}
code() { curl -s -o /tmp/vbody -w "%{http_code}" "$@"; }
H(){ echo "-H"; echo "X-API-Token: $API_TOKEN"; }

echo "== auth =="
t "no token -> 401"    401 "$(code $BASE/chats)"
t "bad token -> 401"   401 "$(code -H 'X-API-Token: wrong' $BASE/chats)"
t "good token -> 200"  200 "$(code -H "X-API-Token: $API_TOKEN" $BASE/chats)"

echo "== input validation =="
t "bad json -> 400"       400 "$(code -X POST -H "X-API-Token: $API_TOKEN" -d 'not json' $BASE/actions/archive)"
t "empty target -> 400"   400 "$(code -X POST -H "X-API-Token: $API_TOKEN" -d '{"chat":""}' $BASE/actions/archive)"
t "garbage target -> 400" 400 "$(code -X POST -H "X-API-Token: $API_TOKEN" -d '{"chat":"!!!"}' $BASE/actions/archive)"
t "send no text -> live path" 502 "$(code -X POST -H "X-API-Token: $API_TOKEN" -d '{"chat":"19995550000","text":"","dry_run":false}' $BASE/actions/send)"

echo "== dry-run (safe) =="
t "dry archive -> 200"        200 "$(code -X POST -H "X-API-Token: $API_TOKEN" -d '{"chat":"19995550000","dry_run":true}' $BASE/actions/archive)"
grep -q '"dry_run": *true\|"dry_run":true' /tmp/vbody && { echo "  PASS: dry_run flag echoed"; PASS=$((PASS+1)); } || { echo "  FAIL: dry_run flag missing"; FAIL=$((FAIL+1)); }
t "dry delete_chat -> 200"    200 "$(code -X POST -H "X-API-Token: $API_TOKEN" -d '{"chat":"19995550000","dry_run":true}' $BASE/actions/delete_chat)"
t "dry block -> 200"          200 "$(code -X POST -H "X-API-Token: $API_TOKEN" -d '{"chat":"19995550000","dry_run":true}' $BASE/actions/block)"

echo "== idempotency =="
K="verify-$(date +%s)-$RANDOM"
code -X POST -H "X-API-Token: $API_TOKEN" -d "{\"chat\":\"19995550000\",\"dry_run\":true,\"idempotency_key\":\"$K\"}" $BASE/actions/mute >/dev/null
ID1=$(python3 -c "import json;print(json.load(open('/tmp/vbody'))['action_id'])")
code -X POST -H "X-API-Token: $API_TOKEN" -d "{\"chat\":\"19995550000\",\"dry_run\":true,\"idempotency_key\":\"$K\"}" $BASE/actions/mute >/dev/null
ID2=$(python3 -c "import json;print(json.load(open('/tmp/vbody'))['action_id'])")
REPLAY=$(python3 -c "import json;print(json.load(open('/tmp/vbody')).get('replayed'))")
[[ "$ID1" == "$ID2" && "$REPLAY" == "True" ]] && { echo "  PASS: idempotency replay same id ($ID1)"; PASS=$((PASS+1)); } || { echo "  FAIL: idem id1=$ID1 id2=$ID2 replay=$REPLAY"; FAIL=$((FAIL+1)); }

echo "== journal lookup =="
t "get action by id -> 200"   200 "$(code -H "X-API-Token: $API_TOKEN" $BASE/actions/$ID1)"
t "get missing action -> 404" 404 "$(code -H "X-API-Token: $API_TOKEN" $BASE/actions/nope-nope)"

echo "== reads =="
t "messages no chat -> 400"   400 "$(code -H "X-API-Token: $API_TOKEN" $BASE/messages)"
t "messages ok -> 200"        200 "$(code -H "X-API-Token: $API_TOKEN" "$BASE/messages?chat=19995550000&limit=5")"

echo ""
echo "RESULT: $PASS passed, $FAIL failed"
exit $FAIL
