#!/usr/bin/env python3
# Paced cleanup of stale UNSAVED-number WhatsApp chats (Tier 1: ghosts + silent >1yr).
# All targets are already backed up in data/unsaved-numbers-backup.csv.
# Safe by default: prints the plan and exits unless --go is passed.
#
#   python3 test/cleanup_unsaved.py           # dry preview (no deletes)
#   python3 test/cleanup_unsaved.py --go       # execute, paced ~1s apart
import json, os, sys, time, urllib.request, urllib.error

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BASE = "http://127.0.0.1:8788"
GO = "--go" in sys.argv

env = {}
for line in open(os.path.join(ROOT, ".env")):
    line = line.strip()
    if "=" in line and not line.startswith("#"):
        k, v = line.split("=", 1); env[k] = v
TOK = env["API_TOKEN"]

def get(path):
    r = urllib.request.Request(BASE + path); r.add_header("X-API-Token", TOK)
    return json.loads(urllib.request.urlopen(r, timeout=30).read())

def post(path, body):
    d = json.dumps(body).encode()
    r = urllib.request.Request(BASE + path, data=d, method="POST")
    r.add_header("X-API-Token", TOK); r.add_header("Content-Type", "application/json")
    try:
        return json.loads(urllib.request.urlopen(r, timeout=30).read())
    except urllib.error.HTTPError as e:
        return json.loads(e.read())

chats = get("/chats?limit=500")["chats"]
groups = {g["jid"] for g in get("/groups")["groups"]}
dated = [c for c in chats if c.get("last_ts")]
NOW = max(c["last_ts"] for c in dated)

def days(c):
    return (NOW - c["last_ts"]) / 86400000.0 if c.get("last_ts") else 9999

def unsaved(c):
    if c["jid"] in groups or c["jid"].endswith(("@g.us", "@newsletter", "@broadcast")):
        return False
    disp = (c.get("display") or c.get("name") or "").strip()
    return (disp == "" or disp.startswith("+") or disp.replace(" ", "").lstrip("+").isdigit()
            or disp.endswith("@lid") or disp == c["jid"])

# Tier 1 = ghosts (no last_ts) or silent > 1 year
t1 = [c for c in chats if unsaved(c) and days(c) > 365]
print(f"Tier 1 unsaved-number chats (ghost + >1yr): {len(t1)}")
if not GO:
    print("DRY RUN — pass --go to delete. All targets are backed up in data/unsaved-numbers-backup.csv")
    sys.exit(0)

ok = fail = 0
for i, c in enumerate(t1, 1):
    d = post("/actions/delete_chat", {"chat": c["jid"], "dry_run": False})
    if d.get("status") == "succeeded":
        ok += 1
    else:
        fail += 1
        print(f"  fail: {c.get('display') or c['jid']}: {d.get('error', d.get('status'))}")
    if i % 10 == 0:
        print(f"  ...{i}/{len(t1)}")
    time.sleep(1)
print(f"DONE: {ok} deleted, {fail} failed of {len(t1)}")
