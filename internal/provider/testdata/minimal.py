#!/usr/bin/env python3
"""The smallest conforming program, written against the raw protocol (no
helper): create/read/update/delete on the same file store as backend.py,
not_implemented for everything else. Reads one request per line until stdin
closes and answers one line each, echoing request_id. Used to check that
every optional operation has a working default."""
import json
import os
import sys

base = os.environ["BACKEND_DIR"]
os.makedirs(base, exist_ok=True)


def path(name):
    return os.path.join(base, name + ".json")


def handle(req):
    op = req["op"]
    if op == "create":
        name = req["input"]["name"]
        json.dump({"input": req["input"], "generation": 1}, open(path(name), "w"))
        return {"id": name, "output": {"address": name + ".example", "generation": 1}}
    if op == "read":
        if not os.path.exists(path(req["id"])):
            return {"exists": False}
        obj = json.load(open(path(req["id"])))
        return {"input": obj["input"], "output": {"address": req["id"] + ".example", "generation": obj["generation"]}}
    if op == "update":
        obj = json.load(open(path(req["id"])))
        obj["input"] = req["input"]
        obj["generation"] += 1
        json.dump(obj, open(path(req["id"]), "w"))
        return {"output": {"address": req["id"] + ".example", "generation": obj["generation"]}}
    if op == "delete":
        if os.path.exists(path(req["id"])):
            os.remove(path(req["id"]))
        return {}
    return {"not_implemented": True}


for line in sys.stdin:
    if not line.strip():
        continue
    req = json.loads(line)
    with open(os.path.join(base, "ops.jsonl"), "a") as log:
        log.write(json.dumps(dict(req, pid=os.getpid())) + "\n")
    resp = handle(req)
    resp["request_id"] = req["request_id"]
    print(json.dumps(resp), flush=True)
