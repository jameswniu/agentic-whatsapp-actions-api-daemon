#!/bin/zsh
# Comprehensive NON-MUTATING verifier for the full expanded surface.
# Touches nothing real: reads, dry-runs (never call WhatsApp), and validation
# errors that fire BEFORE any API call. No --go, no live send/typing/presence.
set -u
export PATH="/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
BASE="http://127.0.0.1:8788"
source "$(dirname "$0")/../.env"
H=(-H "X-API-Token: $API_TOKEN")
BOGUS="19995550000"          # not a real contact; dry-run never executes anyway
P=0; F=0
ok(){ echo "  PASS: $1"; P=$((P+1)); }
no(){ echo "  FAIL: $1  (${2:-})"; F=$((F+1)); }
code(){ curl -s -o /tmp/vb -w "%{http_code}" "$@"; }
body(){ cat /tmp/vb; }

# pick a real group for READ-ONLY checks
GJID=$(curl -s "${H[@]}" "$BASE/chats?limit=60" | python3 -c "import sys,json;print(next((c['jid'] for c in json.load(sys.stdin)['chats'] if c['jid'].endswith('@g.us')),''))")

echo "-- auth --"
[[ "$(code "$BASE/chats")" == 401 ]] && ok "no token 401" || no "no token" "$(body)"
[[ "$(code -H 'X-API-Token: x' "$BASE/chats")" == 401 ]] && ok "bad token 401" || no "bad token"
[[ "$(code "${H[@]}" "$BASE/chats")" == 200 ]] && ok "good token 200" || no "good token"

echo "-- reads (all read-only) --"
for path in "/health" "/chats?limit=3" "/messages?chat=$BOGUS&limit=3" "/groups"; do
  [[ "$(code "${H[@]}" "$BASE$path")" == 200 ]] && ok "GET $path" || no "GET $path" "$(body)"
done
[[ -n "$GJID" ]] && { [[ "$(code "${H[@]}" "$BASE/group?chat=$GJID")" == 200 ]] && ok "GET /group (real, read-only)" || no "group info" "$(body)"; }
# invite: 200 (link) or 502 (not admin) both prove the endpoint works, no mutation
[[ -n "$GJID" ]] && { c=$(code "${H[@]}" "$BASE/group/invite?chat=$GJID"); [[ "$c" == 200 || "$c" == 502 ]] && ok "GET /group/invite (read-only)" || no "invite" "$c"; }

echo "-- bad input (400, no mutation) --"
[[ "$(code -X POST "${H[@]}" -d 'nope' "$BASE/actions/archive")" == 400 ]] && ok "invalid json 400" || no "bad json"
[[ "$(code -X POST "${H[@]}" -d '{"chat":""}' "$BASE/actions/archive")" == 400 ]] && ok "empty target 400" || no "empty target"
[[ "$(code -X POST "${H[@]}" -d '{"chat":"!!!"}' "$BASE/actions/archive")" == 400 ]] && ok "garbage target 400" || no "garbage"

echo "-- validation: errors BEFORE any WhatsApp call (no mutation) --"
# these use dry_run:false but the required-param check returns first, so nothing is sent
code -X POST "${H[@]}" -d "{\"chat\":\"$BOGUS\",\"text\":\"\",\"dry_run\":false}" "$BASE/actions/send"; grep -q required <(body) && ok "send needs text" || no "send validation" "$(body)"
code -X POST "${H[@]}" -d "{\"chat\":\"$BOGUS\",\"dry_run\":false}" "$BASE/actions/edit_message"; grep -q required <(body) && ok "edit needs msg_id/text" || no "edit validation" "$(body)"
code -X POST "${H[@]}" -d "{\"chat\":\"$BOGUS\",\"dry_run\":false}" "$BASE/actions/react"; grep -q required <(body) && ok "react needs msg_id" || no "react validation" "$(body)"
code -X POST "${H[@]}" -d '{"dry_run":false}' "$BASE/actions/group_create"; grep -q required <(body) && ok "group_create needs name/participants" || no "gc validation" "$(body)"
code -X POST "${H[@]}" -d '{"dry_run":false}' "$BASE/actions/group_join"; grep -q required <(body) && ok "group_join needs code" || no "gj validation" "$(body)"

echo "-- dry-run wiring for every destructive op (200 + dry_run:true, ZERO mutation) --"
for ep in archive unarchive mute unmute delete_chat delete_message block unblock mark_read mark_unread \
          star unstar pin unpin follow mute_newsletter unmute_newsletter \
          group_leave group_add group_remove group_promote group_demote group_name group_topic; do
  code -X POST "${H[@]}" -d "{\"chat\":\"$BOGUS\",\"dry_run\":true}" "$BASE/actions/$ep"
  python3 -c "import json;d=json.load(open('/tmp/vb'));exit(0 if d.get('dry_run')==True else 1)" 2>/dev/null \
    && ok "dry $ep" || no "dry $ep" "$(body)"
done

echo "-- idempotency (dry-run, no mutation) --"
K="verify-$$-$RANDOM"
code -X POST "${H[@]}" -d "{\"chat\":\"$BOGUS\",\"dry_run\":true,\"idempotency_key\":\"$K\"}" "$BASE/actions/mute"
ID1=$(python3 -c "import json;print(json.load(open('/tmp/vb'))['action_id'])")
code -X POST "${H[@]}" -d "{\"chat\":\"$BOGUS\",\"dry_run\":true,\"idempotency_key\":\"$K\"}" "$BASE/actions/mute"
python3 -c "import json;d=json.load(open('/tmp/vb'));exit(0 if d['action_id']=='$ID1' and d.get('replayed') else 1)" && ok "idempotency replay" || no "idempotency"

echo "-- journal --"
[[ "$(code "${H[@]}" "$BASE/actions/$ID1")" == 200 ]] && ok "get action 200" || no "get action"
[[ "$(code "${H[@]}" "$BASE/actions/nope")" == 404 ]] && ok "missing action 404" || no "missing"

echo ""
echo "RESULT: $P passed, $F failed"
exit $F
