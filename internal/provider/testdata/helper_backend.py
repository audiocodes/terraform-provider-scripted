#!/usr/bin/env python3
"""A backend written on the helper, for acceptance tests of the helper itself.

Objects are JSON files under $HB_DIR. Knobs:
  HB_SLEEP  seconds every create takes (to observe concurrency)
"""
import json
import os
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "helpers", "python"))
from scripted import resource  # noqa: E402

r = resource()
BASE = os.environ["HB_DIR"]


def path(name):
    return os.path.join(BASE, name + ".json")


def load(name):
    with open(path(name)) as f:
        return json.load(f)


def store(name, obj):
    os.makedirs(BASE, exist_ok=True)
    with open(path(name), "w") as f:
        json.dump(obj, f)


@r.create
def create(req):
    time.sleep(float(os.environ.get("HB_SLEEP", "0")))
    name = req.input["name"]
    store(name, {"input": req.input, "generation": 1})
    return r.ok(id=name, output={"generation": 1})


@r.read
def read(req):
    if not os.path.exists(path(req.id)):
        return r.gone()
    obj = load(req.id)
    return r.ok(input=req.managed(obj["input"]), output={"generation": obj["generation"]})


@r.update
def update(req):
    obj = load(req.id)
    obj["input"] = req.input
    obj["generation"] += 1
    store(req.id, obj)
    return r.ok(output={"generation": obj["generation"]})


@r.delete
def delete(req):
    if os.path.exists(path(req.id)):
        os.remove(path(req.id))


if __name__ == "__main__":
    r.main()
