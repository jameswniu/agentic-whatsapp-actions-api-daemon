#!/usr/bin/env python3
# READ-ONLY preview of the "Bucket A" cut, using REAL last-message content
# (the chat-list last_text field is unreliable / often empty for @lid chats).
#
# Target = unsaved-number chats, silent > 6 months, whose ACTUAL last message
# is empty or <= 2 words (pure ghosts / pleasantries). Plus T3 junk (+0, empty @lid <90d).
# Single-process (no curl spawn throttle). NEVER deletes.
import json, os, sys, time, urllib.request, urllib.parse

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BASE = "http://127.0.0.1:8788"

env = {}
for line in open(os.path.join(ROOT, ".env")):
    line = line.strip()
    if "=" in line and not line.startswith("#"):
        k, v = line.split("=", 1); env[k] = v
TOK = env["API_TOKEN"]

def get(path):
    r = urllib.request.Request(BASE + path); r.add_header("X-API-Token", TOK)
    return json.loads(urllib.request.urlopen(r, timeout=30).read())

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

def real_last_text(jid):
    """Pull the actual most-recent message text for a chat."""
    try:
        q = urllib.parse.quote(jid, safe="")
        d = get(f"/messages?chat={q}&limit=5")
        msgs = d.get("messages") or []
        for m in msgs:  # messages returned newest-first
            t = (m.get("text") or "").strip()
            if t:
                return t
        return ""
    except Exception as e:
        return f"<err:{e}>"

def label(c):
    return (c.get("display") or c.get("name") or c["jid"]).strip() or c["jid"]

# Candidate set: unsaved, >6mo silent (>182d)
cands = [c for c in chats if unsaved(c) and days(c) > 182]

buckA, keep = [], []
for c in cands:
    d = days(c)
    txt = real_last_text(c["jid"])
    wc = len(txt.split())
    if txt.startswith("<err:") is False and (txt == "" or wc <= 2):
        buckA.append((d, c, txt))
    else:
        keep.append((d, c, txt))
    time.sleep(0.05)

# T3 junk: <90d, literal +0 or empty-real-text @lid ghost
t3junk = []
for c in chats:
    if not unsaved(c) or days(c) > 90:
        continue
    disp = (c.get("display") or c.get("name") or "").strip()
    txt = real_last_text(c["jid"])
    if disp == "+0" or (c["jid"].endswith("@lid") and txt == ""):
        t3junk.append((days(c), c, txt))
    time.sleep(0.05)

buckA.sort(key=lambda x: -x[0]); keep.sort(key=lambda x: -x[0]); t3junk.sort(key=lambda x: -x[0])

print("=" * 74)
print(f"BUCKET A — DELETE (>6mo silent, real last msg empty or <=2 words): {len(buckA)}")
print("=" * 74)
for d, c, t in buckA:
    print(f"  {int(d//30):>2}mo  {label(c):<26} {('«'+t+'»') if t else '(truly empty)'}")

print()
print("=" * 74)
print(f"T3 JUNK — DELETE (+0 / empty @lid <90d): {len(t3junk)}")
print("=" * 74)
for d, c, t in t3junk:
    print(f"  {int(d):>3}d  {label(c):<26} {('«'+t+'»') if t else '(truly empty)'}")

print()
print("=" * 74)
print(f"KEEP — has real content (>6mo but substantive): {len(keep)}")
print("=" * 74)
for d, c, t in keep:
    snip = (t[:50] + "…") if len(t) > 51 else t
    print(f"  {int(d//30):>2}mo  {label(c):<26} «{snip}»")

print()
print(f"TOTAL to delete: {len(buckA) + len(t3junk)}   |   spared (has content): {len(keep)}")
print("All unsaved numbers are in data/unsaved-numbers-backup.csv. READ-ONLY — nothing deleted.")
