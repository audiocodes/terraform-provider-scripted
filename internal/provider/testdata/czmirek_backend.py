#!/usr/bin/env python3
"""A czmirek/script-style driver for the same fake backend as backend.py.

Used to set up state under czmirek/script so the move to scripted_resource
can be tested. Usage: czmirek_backend.py <action> <name> <json data>
where action is create|read|update|delete|target_state. Prints
{"id": ..., "resource": "<json string>"} like that provider expects.
"""
import json
import os
import sys

action, name, data = sys.argv[1], sys.argv[2], json.loads(sys.argv[3])
# czmirek/script passes no environment of its own, so the storage directory
# is the working_dir it runs the script in.
base = os.environ.get("BACKEND_DIR") or os.getcwd()
os.makedirs(base, exist_ok=True)
path = os.path.join(base, name + ".json")


def resource_string(obj):
    return json.dumps(dict(obj["input"], generation=obj["generation"]), sort_keys=True)


def emit(obj):
    print(json.dumps({"id": name, "resource": resource_string(obj)}))


if action == "create":
    obj = {"input": data, "generation": 1}
    json.dump(obj, open(path, "w"))
    emit(obj)
elif action == "read":
    if not os.path.exists(path):
        print(json.dumps({"id": "", "resource": ""}))
    else:
        emit(json.load(open(path)))
elif action == "target_state":
    # Unlike the other actions this one prints the bare resource string.
    obj = json.load(open(path))
    print(resource_string({"input": data, "generation": obj["generation"]}), end="")
elif action == "update":
    obj = json.load(open(path))
    obj["input"] = data
    obj["generation"] += 1
    json.dump(obj, open(path, "w"))
elif action == "delete":
    if os.path.exists(path):
        os.remove(path)
