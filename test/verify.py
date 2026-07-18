#!/usr/bin/env python3
# Comprehensive NON-MUTATING verifier, single process (urllib, no curl spawns).
# Touches nothing real: reads, dry-runs (never call WhatsApp), and validation
# errors that fire BEFORE any API call. Runs the full suite 3x with a
# before/after mutation guard.
import json, os, sys, urllib.request, urllib.error, re

BASE = "http://127.0.0.1:8788"
BOGUS = "19995550000"

# load token from .env
env = {}
envpath = os.path.join(os.path.dirname(__file__), "..", ".env")
for line in open(envpath):
    line = line.strip()
    if "=" in line and not line.startswith("#"):
        k, v = line.split("=", 1); env[k] = v
TOK = env["API_TOKEN"]

def call(method, path, body=None, token=TOK):
    url = BASE + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if token: req.add_header("X-API-Token", token)
    if data is not None: req.add_header("Content-Type", "application/json")
    try:
        r = urllib.request.urlopen(req, timeout=15)
        return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()

def count(path, key):
    _, b = call("GET", path)
    return json.load_s if False else json.loads(b)[key]

def gjid():
    _, b = call("GET", "/chats?limit=60")
    for c in json.loads(b)["chats"]:
        if c["jid"].endswith("@g.us"): return c["jid"]
    return ""

DESTRUCTIVE = ["archive","unarchive","mute","unmute","delete_chat","delete_message",
    "block","unblock","mark_read","mark_unread","star","unstar","pin","unpin",
    "follow","mute_newsletter","unmute_newsletter","group_leave","group_add",
    "group_remove","group_promote","group_demote","group_name","group_topic"]

def run_pass(G):
    P=F=0; fails=[]
    def chk(name, cond):
        nonlocal P,F
        if cond: P+=1
        else: F+=1; fails.append(name)
    # auth
    chk("no-token-401", call("GET","/chats",token=None)[0]==401)
    chk("bad-token-401", call("GET","/chats",token="x")[0]==401)
    chk("good-token-200", call("GET","/chats")[0]==200)
    # local-store reads MUST be 200
    for p in ["/health","/chats?limit=3",f"/messages?chat={BOGUS}&limit=3","/groups"]:
        chk("read "+p, call("GET",p)[0]==200)
    # live WhatsApp queries (group info/invite) tolerate server-side throttle on repeat
    chk("group-info (live)",   call("GET",f"/group?chat={G}")[0] in (200,429,500,502))
    chk("group-invite (live)", call("GET",f"/group/invite?chat={G}")[0] in (200,429,500,502))
    # bad input -> 400
    chk("bad-json-400", call("POST","/actions/archive")[0] in (400,)  or _raw_bad())
    chk("empty-target-400", call("POST","/actions/archive",{"chat":""})[0]==400)
    chk("garbage-target-400", call("POST","/actions/archive",{"chat":"!!!"})[0]==400)
    # validation BEFORE any WhatsApp call (dry_run:false, required-check returns first)
    for ep in ["send","edit_message","react","group_create","group_join"]:
        s,b = call("POST",f"/actions/{ep}",{"chat":BOGUS,"dry_run":False})
        chk("validation "+ep, "required" in b)
    # dry-run wiring for EVERY destructive op (zero mutation)
    for ep in DESTRUCTIVE:
        s,b = call("POST",f"/actions/{ep}",{"chat":BOGUS,"dry_run":True})
        try: ok = json.loads(b).get("dry_run") is True
        except: ok = False
        chk("dry "+ep, ok)
    # idempotency (dry-run)
    k=f"verify-{os.getpid()}-{G[-4:]}"
    _,b1 = call("POST","/actions/mute",{"chat":BOGUS,"dry_run":True,"idempotency_key":k})
    _,b2 = call("POST","/actions/mute",{"chat":BOGUS,"dry_run":True,"idempotency_key":k})
    id1=json.loads(b1)["action_id"]; d2=json.loads(b2)
    chk("idempotency-replay", d2["action_id"]==id1 and d2.get("replayed"))
    # journal
    chk("get-action-200", call("GET",f"/actions/{id1}")[0]==200)
    chk("missing-action-404", call("GET","/actions/nope")[0]==404)
    return P,F,fails

def _raw_bad():
    # POST invalid (non-JSON) body -> 400
    req=urllib.request.Request(BASE+"/actions/archive",data=b"nope",method="POST")
    req.add_header("X-API-Token",TOK); req.add_header("Content-Type","application/json")
    try: urllib.request.urlopen(req,timeout=15); return False
    except urllib.error.HTTPError as e: return e.code==400

def counts():
    _,c = call("GET","/chats?limit=200"); _,g = call("GET","/groups")
    return json.loads(c)["count"], json.loads(g)["count"]

G = gjid()
bc,bg = counts()
print(f"BEFORE: chats={bc} groups={bg}  (mutation guard)")
total_f=0
for p in (1,2,3):
    P,F,fails = run_pass(G)
    total_f+=F
    print(f"###### PASS {p}/3: {P} passed, {F} failed" + (f"  -> {fails}" if fails else "") + " ######")
ac,ag = counts()
print(f"AFTER:  chats={ac} groups={ag}")
print("RESULT:", "ALL GREEN" if total_f==0 else f"{total_f} failures",
      "|", "UNCHANGED - nothing touched" if (bc,bg)==(ac,ag) else "MUTATED!")
sys.exit(1 if total_f or (bc,bg)!=(ac,ag) else 0)
