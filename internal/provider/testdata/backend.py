#!/usr/bin/env python3
"""Fake backend for the acceptance tests.

Each managed object is a JSON file ``$BACKEND_DIR/<id>.json`` holding the
input it was last written with and a generation counter. Every request the
provider sends is appended verbatim (plus the serving pid) to
``$BACKEND_DIR/ops.jsonl`` so tests can assert which operations ran, with
what, and from how many processes.

Knobs, all environment variables:
  BACKEND_DIR         where objects live (required)
  BACKEND_TAG         echoed into output.tag, to observe environment merging
  BACKEND_FAIL        name of an op on which the program exits with status 2
  BACKEND_SLEEP       seconds to sleep before answering any op
  BACKEND_SLEEP_NAME  only sleep for the object with this name
  BACKEND_GARBAGE     name of an op after whose JSON a stray line is printed
  BACKEND_SILENT      name of an op that prints nothing
  BACKEND_CONCURRENT  "1": serve requests in threads
  BACKEND_NOTIMPL     name of an op answered with not_implemented
  BACKEND_NEWID       name of an op whose answer carries a different id
  BACKEND_BREAK_PROMISE "1": create returns output other than plan promised
  BACKEND_ECHO_ENV    name of an environment variable whose presence is
                      reported in output.env_present

Reads one request per line until stdin closes and answers one line each,
echoing request_id.
"""
import json
import os
import sys
import threading
import time

LOG_LOCK = threading.Lock()


class Failure(Exception):
    pass


def serve(req):
    """Handle one request. Returns (stdout text or None, exit code)."""
    op = req["op"]
    base = os.environ["BACKEND_DIR"]
    os.makedirs(base, exist_ok=True)
    with LOG_LOCK, open(os.path.join(base, "ops.jsonl"), "a") as log:
        log.write(json.dumps(dict(req, pid=os.getpid())) + "\n")

    if os.environ.get("BACKEND_SLEEP"):
        only = os.environ.get("BACKEND_SLEEP_NAME")
        name = req.get("id") or (req.get("input") or {}).get("name")
        if not only or only == name:
            time.sleep(float(os.environ["BACKEND_SLEEP"]))
    if os.environ.get("BACKEND_FAIL") == op:
        print("forced failure of " + op, file=sys.stderr)
        sys.stderr.flush()
        os._exit(2)
    if os.environ.get("BACKEND_SILENT") == op:
        # Answer, but with nothing in it.
        return json.dumps(with_meta({}, req)), 0

    if os.environ.get("BACKEND_NOTIMPL") == op:
        return json.dumps(with_meta({"not_implemented": True}, req)), 0
    handler = HANDLERS.get(op)
    if handler is None:
        # The rule for anything this script does not know.
        return json.dumps(with_meta({"not_implemented": True}, req)), 0
    try:
        resp = handler(req, base)
    except Failure as e:
        return json.dumps(with_meta({"errors": [str(e)]}, req)), 1
    resp = dict(resp or {})
    if os.environ.get("BACKEND_NEWID") == op:
        resp["id"] = "changed"
    if os.environ.get("BACKEND_BREAK_PROMISE") == "1" and op == "create":
        resp["output"] = {"address": "not-what-was-promised"}
    text = json.dumps(with_meta(resp, req))
    if os.environ.get("BACKEND_GARBAGE") == op:
        text += "\ndebug: all good"
    return text, 0


def with_meta(resp, req):
    resp = dict(resp or {})
    if req.get("request_id") is not None:
        resp["request_id"] = req["request_id"]
    return resp


def plan(req, base):
    resp = {}
    prior, new = req.get("prior_input") or {}, req.get("input") or {}
    ctx = req.get("context") or {}
    if req["action"] in ("create", "update") and isinstance(new.get("size"), (int, float)) and new["size"] < 0:
        resp["errors"] = ["size must be >= 0"]
    if req["action"] == "update" and prior.get("immutable") != new.get("immutable"):
        resp["requires_replace"] = True
    if req["action"] == "update" and ctx.get("replace_on_change"):
        # Asks for replacement on any update, including context-only ones.
        resp["requires_replace"] = True
    if "warn" in new:
        resp["warnings"] = [str(new["warn"])]
    if req["action"] == "create" and new.get("known_output") and "name" in new:
        # Output that can be promised before apply.
        resp["output"] = output(new["name"], 1, 0)
    if "misplaced" in new:
        # A field that means nothing for plan, to see the provider warn.
        resp["exists"] = True
    if new.get("plan_private"):
        resp["private"] = {"from_plan": req["action"]}
    return resp


def create(req, base):
    name = req["input"]["name"]
    path = obj_path(base, name)
    if os.path.exists(path):
        raise Failure("object %r already exists" % name)
    store(path, {"input": req["input"], "generation": 1})
    return {"id": name, "output": output(name, 1, 0), "private": {"reads": 0}}


def read(req, base):
    path = obj_path(base, req["id"])
    if not os.path.exists(path):
        return {"exists": False}
    obj = load(path)
    # Report only the keys Terraform manages, the way a real backend script
    # would filter a GET that returns far more than was set.
    prior = req.get("input")
    if isinstance(prior, dict):
        live = {k: obj["input"].get(k) for k in prior}
    else:
        live = obj["input"]
    reads = (req.get("private") or {}).get("reads", 0)
    return {"input": live, "output": output(req["id"], obj["generation"], reads), "private": {"reads": reads + 1}}


def update(req, base):
    path = obj_path(base, req["id"])
    obj = load(path)
    obj["input"] = req["input"]
    obj["generation"] += 1
    store(path, obj)
    return {"output": output(req["id"], obj["generation"], (req.get("private") or {}).get("reads", 0))}


def delete(req, base):
    path = obj_path(base, req["id"])
    if os.path.exists(path):
        os.remove(path)


def do_import(req, base):
    path = obj_path(base, req["id"])
    if not os.path.exists(path):
        raise Failure("no object %r to import" % req["id"])
    obj = load(path)
    return {"id": req["id"], "input": obj["input"], "output": output(req["id"], obj["generation"], 0),
            "private": {"reads": 0}}


def move(req, base):
    src = req["source"]
    if src["type"] != "script":
        return {"not_implemented": True}
    # czmirek/script state: the id is the object name and the files are
    # shared with czmirek_backend.py, so just adopt the stored object.
    name = src["state"]["id"]
    obj = load(obj_path(base, name))
    return {"id": name, "input": obj["input"], "output": output(name, obj["generation"], 0),
            "context": {"adopted": True}, "private": {"reads": 0}}


def data(req, base):
    inp = req.get("input") or {}
    out = {"found": False, "echo": inp, "provider_input": req.get("provider_input"), "context": req.get("context")}
    if "name" in inp and os.path.exists(obj_path(base, inp["name"])):
        out.update(found=True, generation=load(obj_path(base, inp["name"]))["generation"])
    return {"output": out}


HANDLERS = {
    "plan": plan,
    "create": create,
    "read": read,
    "update": update,
    "delete": delete,
    "import": do_import,
    "move": move,
    "data": data,
}


def output(name, generation, reads_seen):
    out = {"address": name + ".example", "generation": generation, "reads_seen": reads_seen}
    if "BACKEND_TAG" in os.environ:
        out["tag"] = os.environ["BACKEND_TAG"]
    if "BACKEND_ECHO_ENV" in os.environ:
        out["env_present"] = os.environ["BACKEND_ECHO_ENV"] in os.environ
    return out


def obj_path(base, name):
    return os.path.join(base, name + ".json")


def load(path):
    with open(path) as f:
        return json.load(f)


def store(path, obj):
    with open(path, "w") as f:
        json.dump(obj, f)


OUT_LOCK = threading.Lock()


def answer(line):
    req = json.loads(line)
    text, code = serve(req)
    with OUT_LOCK:
        sys.stdout.write(text + "\n")
        sys.stdout.flush()


def main():
    threads = []
    for line in sys.stdin:
        if not line.strip():
            continue
        if os.environ.get("BACKEND_CONCURRENT") == "1":
            t = threading.Thread(target=answer, args=(line,))
            t.start()
            threads.append(t)
        else:
            answer(line)
    for t in threads:
        t.join()


if __name__ == "__main__":
    main()
