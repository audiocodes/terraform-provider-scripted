#!/usr/bin/env python3
"""A "memory" resource, after apparentlymart/terraform-provider-memory,
written against the raw protocol to show what the wire format looks like
without the helper (manage.py shows the helper).

    resource "scripted_resource" "version" {
      program = ["python3", "${path.module}/memory.py"]
      input   = { new_value = var.new_password_version }   # may be null
    }
    # scripted_resource.version.output.value is the remembered value

The value is remembered until a non-null new_value is given; a null new_value
leaves it as it is. Nothing outside Terraform holds the value, so the state
(prior_output) is the source of truth and the plan hook promises the result,
making output.value known at plan time for whatever depends on it.

Protocol: one JSON request per line on stdin until it closes; one JSON
response per line on stdout, echoing request_id; nothing else on stdout.
"""
import json
import sys


def handle(req):
    new_value = (req.get("input") or {}).get("new_value")
    prior = req.get("prior_output") or {}
    value = prior.get("value", "") if new_value is None else new_value

    op = req["op"]
    if op == "plan":
        if req["action"] in ("create", "update"):
            return {"output": {"value": value}}
        return {}
    if op == "create":
        return {"id": "memory", "output": {"value": value}}
    if op == "read":
        return {"output": prior}
    if op == "update":
        return {"output": {"value": value}}
    if op == "delete":
        return {}
    return {"not_implemented": True}


for line in sys.stdin:
    if not line.strip():
        continue
    request = json.loads(line)
    response = handle(request)
    response["request_id"] = request["request_id"]
    print(json.dumps(response), flush=True)
