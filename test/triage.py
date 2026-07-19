#!/usr/bin/env python3
# READ-ONLY triage: surface stale/trash candidates. Deletes nothing.
import json, os, urllib.request, datetime

BASE="http://127.0.0.1:8788"
env={}
for line in open(os.path.join(os.path.dirname(__file__),"..",".env")):
    line=line.strip()
    if "=" in line and not line.startswith("#"): k,v=line.split("=",1); env[k]=v
TOK=env["API_TOKEN"]

def get(path):
    req=urllib.request.Request(BASE+path); req.add_header("X-API-Token",TOK)
    return json.loads(urllib.request.urlopen(req,timeout=20).read())

NOW=1784000000000  # ~2026-07 baseline in ms; recomputed from newest chat below
chats=get("/chats?limit=500")["chats"]
groups={g["jid"]:g for g in get("/groups")["groups"]}

# newest activity = "now" reference (store has no wall clock in this script)
NOW=max((c["last_ts"] for c in chats if c["last_ts"]), default=NOW)

def days(ts):
    return (NOW-ts)/86400000.0 if ts else 9999

def kind(jid):
    if jid.endswith("@g.us"): return "group"
    if jid.endswith("@newsletter"): return "newsletter"
    if jid.endswith("@broadcast"): return "broadcast"
    if jid.endswith("@lid") or jid.endswith("@s.whatsapp.net"): return "dm"
    return "other"

rows=[]
for c in chats:
    j=c["jid"]; k=kind(j); d=days(c["last_ts"])
    g=groups.get(j)
    rows.append({"jid":j,"name":c.get("display") or c.get("name") or j,"kind":k,
                 "days":d,"participants":(g or {}).get("participants")})

def show(title, items, n=25):
    print(f"\n=== {title} ({len(items)}) ===")
    for r in items[:n]:
        p=f" | {r['participants']}p" if r["participants"] is not None else ""
        print(f"  {r['days']:5.0f}d  {r['kind']:10} {r['name'][:44]:44}{p}")
    if len(items)>n: print(f"  ... and {len(items)-n} more")

print(f"Total chats in store: {len(rows)} | joined groups: {len(groups)}")
print(f"(days = time since last message; newest chat = 0d reference)")

# 1. Stalest groups (no activity in a long time)
stale_groups=sorted([r for r in rows if r["kind"]=="group" and r["days"]>60], key=lambda r:-r["days"])
show("STALE GROUPS (>60d silent) — candidates to leave", stale_groups)

# 2. Newsletters (followed channels — often set-and-forget)
news=sorted([r for r in rows if r["kind"]=="newsletter"], key=lambda r:-r["days"])
show("NEWSLETTERS followed — candidates to unfollow", news)

# 3. Tiny groups (2-3 people, stale) — likely dead 1:1-ish groups
tiny=sorted([r for r in rows if r["kind"]=="group" and (r["participants"] or 99)<=3 and r["days"]>30], key=lambda r:-r["days"])
show("TINY STALE GROUPS (<=3 people, >30d) — likely dead", tiny)

# 4. Stalest DMs
stale_dm=sorted([r for r in rows if r["kind"]=="dm" and r["days"]>120], key=lambda r:-r["days"])
show("STALE DMs (>120d silent)", stale_dm)

# 5. Broadcast / odd entries
odd=[r for r in rows if r["kind"] in ("broadcast","other")]
show("BROADCAST / OTHER entries", odd)
