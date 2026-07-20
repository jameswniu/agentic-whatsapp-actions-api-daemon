#!/usr/bin/env python3
# Paced delete of the "Bucket A" cut computed by preview_bucketA.py:
#   unsaved-number chats, >6mo silent, REAL last message empty or <=2 words,
#   plus T3 junk (+0 / empty @lid <90d). All backed up in unsaved-numbers-backup.csv.
# Same content-aware filter as the preview. Dry unless --go. Paced ~1s apart.
import json, os, sys, time, urllib.request, urllib.parse, urllib.error

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
NOW = max(c["last_ts"] for c in chats if c.get("last_ts"))

def days(c):
    return (NOW - c["last_ts"]) / 86400000.0 if c.get("last_ts") else 9999

def unsaved(c):
    if c["jid"] in groups or c["jid"].endswith(("@g.us", "@newsletter", "@broadcast")):
        return False
    disp = (c.get("display") or c.get("name") or "").strip()
    return (disp == "" or disp.startswith("+") or disp.replace(" ", "").lstrip("+").isdigit()
            or disp.endswith("@lid") or disp == c["jid"])

def real_last_text(jid):
    try:
        q = urllib.parse.quote(jid, safe="")
        for m in (get(f"/messages?chat={q}&limit=5").get("messages") or []):
            t = (m.get("text") or "").strip()
            if t:
                return t
        return ""
    except Exception:
        return "<err>"

def label(c):
    return (c.get("display") or c.get("name") or c["jid"]).strip() or c["jid"]

targets = []
for c in chats:
    if unsaved(c) and days(c) > 182:
        t = real_last_text(c["jid"])
        if t != "<err>" and (t == "" or len(t.split()) <= 2):
            targets.append(c)
    time.sleep(0.03)
for c in chats:
    if unsaved(c) and days(c) <= 90:
        disp = (c.get("display") or c.get("name") or "").strip()
        if disp == "+0" or (c["jid"].endswith("@lid") and real_last_text(c["jid"]) == ""):
            targets.append(c)
    time.sleep(0.03)

# dedupe by jid
seen, uniq = set(), []
for c in targets:
    if c["jid"] not in seen:
        seen.add(c["jid"]); uniq.append(c)
targets = uniq

print(f"Bucket A + T3 junk targets: {len(targets)}")
if not GO:
    for c in targets:
        print("  would delete:", label(c))
    print("DRY RUN — pass --go to delete. Backed up in data/unsaved-numbers-backup.csv")
    sys.exit(0)

ok = fail = 0
for i, c in enumerate(targets, 1):
    d = post("/actions/delete_chat", {"chat": c["jid"], "dry_run": False})
    if d.get("status") == "succeeded":
        ok += 1
    else:
        fail += 1
        print(f"  fail: {label(c)}: {d.get('error', d.get('status'))}")
    if i % 10 == 0:
        print(f"  ...{i}/{len(targets)}")
    time.sleep(1)
print(f"DONE: {ok} deleted, {fail} failed of {len(targets)}")
