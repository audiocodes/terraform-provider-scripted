#!/usr/bin/env python3
"""Example program: an object behind a JSON REST API, standard library only.

    provider "scripted" {
      program = ["python3", "${path.root}/rest_api.py"]
      input   = { base_url = "https://api.example.com/v1/widgets", token = var.token }
    }
    resource "scripted_resource" "w" {
      input = { name = "w1", size = 2 }
    }

create  -> POST   {base_url}            read   -> GET    {base_url}/{id}
update  -> PUT    {base_url}/{id}       delete -> DELETE {base_url}/{id}

The API is expected to return the object as JSON with an "id" field and the
fields that were sent. Adjust `to_api`/`from_api` for anything else.
"""
import json
import urllib.error
import urllib.request

from scripted import resource

# Handlers run in threads, one per request Terraform has in flight. An
# opener (like a requests.Session) is not thread-safe, so each thread gets
# its own through r.local(); handlers otherwise keep to local variables.
r = resource()
opener = r.local(urllib.request.build_opener)


def call(req, method, path="", body=None):
    api = req.provider_input
    http = urllib.request.Request(
        api["base_url"] + path,
        data=json.dumps(body).encode() if body is not None else None,
        method=method,
        headers={"Authorization": "Bearer " + api["token"], "Content-Type": "application/json"},
    )
    try:
        with opener().open(http, timeout=60) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return None
        r.fail("%s %s failed: HTTP %s %s" % (method, path or "/", e.code, e.read().decode(errors="replace")))


def output_for(obj):
    return {"id": obj["id"], "created_at": obj.get("created_at")}


@r.create
def create(req):
    obj = call(req, "POST", body=req.input)
    return r.ok(id=obj["id"], output=output_for(obj))


@r.read
def read(req):
    obj = call(req, "GET", "/" + req.id)
    if obj is None:
        return r.gone()
    return r.ok(input=req.managed(obj), output=output_for(obj))


@r.update
def update(req):
    obj = call(req, "PUT", "/" + req.id, body=req.input)
    return r.ok(output=output_for(obj))


@r.delete
def delete(req):
    call(req, "DELETE", "/" + req.id)


@r.import_
def import_(req):
    obj = call(req, "GET", "/" + req.id)
    if obj is None:
        r.fail("no object %r" % req.id)
    # Input is not known here: the first plan records it and update finds
    # the object already matching.
    return r.ok(id=req.id, output=output_for(obj))


if __name__ == "__main__":
    r.main()
