#!/usr/bin/env python3
"""Example program for scripted_resource: records stored as JSON files.

Uses the vendored helper (scripted.py, next to this file): handlers are
plain functions, anything not handled answers not_implemented, exceptions
become errors, and `python3 manage.py --op read --id web-1` runs one
operation by hand. Replace the store_* functions with calls to whatever
system you manage and the rest stays the same.

Keep heavy imports (requests, cloud SDKs) inside the handlers that need
them: the program is started afresh for every operation.
"""
import json
import os

from scripted import resource

STORE_DIR = os.environ.get("STORE_DIR", "store")
r = resource()


# --- the system being managed ----------------------------------------------

def store_path(record_id):
    return os.path.join(STORE_DIR, record_id + ".json")


def store_get(record_id):
    try:
        with open(store_path(record_id)) as f:
            return json.load(f)
    except FileNotFoundError:
        return None


def store_put(record_id, record):
    os.makedirs(STORE_DIR, exist_ok=True)
    with open(store_path(record_id), "w") as f:
        json.dump(record, f, indent=2)


def store_delete(record_id):
    try:
        os.remove(store_path(record_id))
    except FileNotFoundError:
        pass


def output_for(record_id, record):
    """What Terraform gets to see under `output`."""
    return {"address": record_id + ".internal.example", "revision": record["revision"]}


# --- the operations ----------------------------------------------------------

@r.plan
def plan(req):
    """Reject bad input early and ask for replacement when `kind` changes."""
    if req.action in ("create", "update") and (req.input.get("size") or 0) < 0:
        r.fail("size must be >= 0")
    if req.action == "update" and req.prior_input.get("kind") != req.input.get("kind"):
        return r.replace()
    if req.action in ("create", "update") and (req.input.get("size") or 0) > 8:
        return r.ok(warnings=["size %s is unusually large" % req.input["size"]])


@r.create
def create(req):
    record_id = req.input["name"]
    if store_get(record_id) is not None:
        r.fail("record %r already exists" % record_id)
    record = {"spec": req.input, "revision": 1}
    store_put(record_id, record)
    return r.ok(id=record_id, output=output_for(record_id, record))


@r.read
def read(req):
    record = store_get(req.id)
    if record is None:
        return r.gone()
    # Only the fields Terraform manages, so that fields the system adds on
    # its own do not show up as drift.
    return r.ok(input=req.managed(record["spec"]), output=output_for(req.id, record))


@r.update
def update(req):
    record = store_get(req.id)
    if record is None:
        r.fail("record %r has disappeared" % req.id)
    # Compare against what is actually there rather than against prior_input,
    # which is null after a default move or import.
    if record["spec"] != req.input:
        record["spec"] = req.input
        record["revision"] += 1
        store_put(req.id, record)
    return r.ok(output=output_for(req.id, record))


@r.delete
def delete(req):
    store_delete(req.id)


@r.import_
def import_(req):
    """`terraform import scripted_resource.x <record id>`."""
    record = store_get(req.id)
    if record is None:
        r.fail("no record %r to import" % req.id)
    return r.ok(id=req.id, input=record["spec"], output=output_for(req.id, record))


@r.move
def move(req):
    """A `moved` block from czmirek/script: adopt the record it managed."""
    if req.source["type"] != "script":
        return r.not_implemented()
    record_id = req.source["state"]["id"]
    record = store_get(record_id)
    if record is None:
        r.fail("the moved resource's record %r does not exist" % record_id)
    return r.ok(id=record_id, input=record["spec"], output=output_for(record_id, record))


@r.data
def data(req):
    """Answer a scripted_data lookup."""
    record = store_get(req.input["name"])
    if record is None:
        return r.ok(output={"exists": False})
    return r.ok(output=dict(output_for(req.input["name"], record), exists=True))


if __name__ == "__main__":
    r.main()
