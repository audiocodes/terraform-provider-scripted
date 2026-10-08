---
page_title: "scripted_resource Resource - scripted"
subcategory: ""
description: |-
  A resource whose whole lifecycle is delegated to a program you write. Terraform does the planning, diffing and state keeping; the program talks to the system being managed. See the protocol section below for what the program receives and must return.
---

# scripted_resource (Resource)

A resource whose whole lifecycle is delegated to a program you write. Terraform does the planning, diffing and state keeping; the program talks to the system being managed. See the protocol section below for what the program receives and must return.

~> **Planning runs the program.** `plan` and `read` execute during
`terraform plan`, so merely planning a configuration runs whatever `program`
points at, with Terraform's environment (cloud credentials, `TF_VAR_*`) in
its own unless the provider sets `inherit_environment = false`. In CI this
means a pull request that changes `program`, or the script it points at,
executes that code on the next plan. Review it as you would any other code
your pipeline runs. See [Security](#security).

## Example Usage

```terraform
# A record kept by manage.py (see below), which stores each record as a JSON
# file. Swap the script for one that talks to a real API and nothing here
# changes.
resource "scripted_resource" "record" {
  program     = ["python3", "${path.module}/manage.py"]
  environment = { STORE_DIR = "${path.module}/store" }
  timeout     = "2m"

  input = {
    name = "web-1"
    kind = "vm" # the script asks for replacement when this changes
    size = 2
  }
}

output "address" {
  value = scripted_resource.record.output.address
}

# Several of them from a map, the usual shape once there is more than one.
variable "records" {
  type = map(object({ kind = string, size = number }))
  default = {
    db-1 = { kind = "vm", size = 4 }
    db-2 = { kind = "container", size = 1 }
  }
}

resource "scripted_resource" "records" {
  for_each = var.records

  program     = ["python3", "${path.module}/manage.py"]
  environment = { STORE_DIR = "${path.module}/store" }

  input = merge(each.value, { name = each.key })
}

# Taking over a record created by hand: `terraform import` with the record id,
# or declaratively:
#
#   import {
#     to = scripted_resource.record
#     id = "web-1"
#   }
#
# manage.py's import handler returns the record's current spec as input, so
# the first plan after the import is clean when the configuration matches.
```

The program it runs, a complete working example that keeps records as JSON
files, written with the vendored helper `scripted.py` (replace the `store_*`
functions with calls to your API):

```python
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
```

Get the helper with `terraform-provider-scripted --emit-helper > scripted.py`
and keep it next to your program. It is standard library only, under the
same permissive Apache-2.0 license as the provider, and stamps the provider version it came from into `HELPER_VERSION`; refresh it
with the same command when you upgrade the provider.

<!-- schema generated by tfplugindocs -->
## Schema

### Optional

- `always_update` (Boolean) Run `update` whenever Terraform applies any change to this resource, not only when `input` changed. Lets the program decide for itself what a change to `program`, `context` or `environment` means, replacement included.
- `context` (Dynamic) Values for the program that are not part of the managed state, such as which object the program is in charge of. Sent as `context` with every operation. Changing it does not run `update` (unless `always_update`).
- `environment` (Map of String) Environment variables for the program, on top of the provider's `environment`.
- `input` (Dynamic) Desired state of the managed resource, in whatever shape the program understands (an object, usually). It is sent to the program on create and update, and the program may refresh it on read so that changes made outside Terraform show up as drift. The update operation only runs when this value changes (see `always_update`). Mark individual values with Terraform's `sensitive()` to hide them in plans.
- `plan_hook` (Boolean) Run the program with `op = "plan"` on every plan, letting it validate the configuration, ask for replacement, supply output values that are known in advance, or add warnings. A program that does not implement `plan` answers `not_implemented`; set this to false to skip the call altogether.
- `program` (List of String) Command and arguments to run for every operation, for example `["python3", "${path.module}/manage.py"]`. Defaults to the provider's `program`. A relative path is resolved against `working_dir`. Changing this never runs the program by itself.
- `sensitive_environment` (Map of String, Sensitive) Like `environment`, but hidden in plan output. Stored in state like any other attribute; the provider's `environment` and `input` are the ways to keep values out of state.
- `timeout` (String) How long the program may take to answer one request for this resource, as a duration such as `"30m"`. Defaults to the provider's `timeout`, else 10 minutes.
- `working_dir` (String) Directory the program runs in. Defaults to the provider's `working_dir`, then Terraform's working directory.

### Read-Only

- `id` (String) Identifier returned by the program on create (or import). Passed back to it on every later operation.
- `output` (Dynamic) Whatever the program returned under `output` from the last create, update or read: identifiers, addresses, and other facts about the managed resource.

## Protocol

The provider starts the program once (the resource's `program`, else the
provider's) and keeps it running for the life of the provider instance —
Terraform starts one per graph walk, so an `apply` that changes something
runs the program two or three times, a `plan` once. It writes
**one JSON request per line** to the program's stdin and reads **one JSON
response per line** from its stdout. Each request has a `request_id`; the
response echoes it, which lets the program answer in any order and lets
several requests be in flight at once. When stdin closes the program must
exit. Standard error is logged (`TF_LOG=DEBUG`) and shown in error messages.

A program using `scripted.py` gets all of this from `r.main()`. Without the
helper, the loop is:

```python
import json, sys
for line in sys.stdin:
    req = json.loads(line)
    resp = handle(req)                      # a dict
    resp["request_id"] = req["request_id"]
    print(json.dumps(resp), flush=True)     # nothing else may go to stdout
```

### Request

```json
{
  "protocol":       1,
  "request_id":     "17",
  "op":             "plan | create | read | update | delete | import | move | data",
  "action":         "create | update | delete | none        (plan only)",
  "id":             "the id returned by create, or null",
  "input":          { "...": "the desired state (see table)" },
  "prior_input":    { "...": "input as recorded in state, or null" },
  "prior_output":   { "...": "output as recorded in state, or null" },
  "input_changed":  true,
  "unknown":        ["paths in input not known yet (plan only)"],
  "private":        { "...": "whatever the program last stored, or null" },
  "source":         { "provider": "...", "type": "...", "schema_version": 0, "state": {} },
  "provider_input": { "...": "the provider block's input, or null" },
  "context":        { "...": "the resource's context, or null" }
}
```

`provider_input` and `context` come with every request. They are for what the
program needs to do its job (server, credentials, which object it manages)
as opposed to `input`, the state it manages: neither takes part in drift
detection, and a change to `context` is recorded without running `update`.
Prefer them to environment variables for anything structured or secret.

| op       | when                                                | `input`                      | response fields read                                                                 |
|----------|-----------------------------------------------------|------------------------------|--------------------------------------------------------------------------------------|
| `plan`   | every plan (`plan_hook`), including destroys        | planned (prior for delete)   | `requires_replace`, `output`, `private`¹, `warnings`, `errors`                       |
| `create` | resource is new or being replaced                   | planned                      | `id` (required), `output`, `private`                                                 |
| `read`   | every refresh                                       | from state                   | `exists`, `input`, `output`, `id`, `private`                                         |
| `update` | `input` changed, or any change with `always_update` | planned                      | `output`, `private`                                                                  |
| `delete` | resource removed from config or being replaced      | from state                   | —                                                                                    |
| `import` | `terraform import` / `import` block                 | — (`id` is the import id)    | `id`, `input`, `output`, `program`, `working_dir`, `environment`, `context`, `private` |
| `move`   | `moved` block from another resource type            | — (`source` has the state)   | same as import                                                                       |
| `data`   | a `scripted_data` data source refreshes             | configured                   | `output`                                                                             |

¹ `private` returned by the plan hook is kept for an existing resource, but
Terraform does not carry plan-time private state into `create`: a plan hook
that stores something for a resource that does not exist yet will not see it
in `create`. Store such things from `create` itself.

Fields a program returns that the operation does not read are reported as a
warning, so that a `requires_replace` from `create` or an `exists` from
`update` does not vanish silently.

### Response

```json
{
  "request_id":       "17",
  "protocol":         1,
  "not_implemented":  true,
  "id":               "required on create and move; read and import may set it",
  "output":           { "...": "anything; becomes the output attribute" },
  "input":            { "...": "read, import, move: the live values of the managed fields" },
  "exists":           false,
  "requires_replace": true,
  "warnings":         ["shown in the plan"],
  "errors":           ["each fails the operation with its own message"],
  "private":          { "...": "stored out of sight and sent back on later operations" },
  "program":          ["import and move: where the program lives, for state"],
  "working_dir":      "...",
  "environment":      { "...": "..." },
  "context":          { "...": "import and move: the context the resource will have" },
  "duration_ms":      123.4
}
```

`duration_ms`, optional, is how long the program spent on the request (the
helper fills it in); the provider logs it as `processing` next to `elapsed`
(send to answer), and the gap between them is queueing, transport and, for a
program's first request, start-up.

### Operations

- **`plan`** runs on every plan when `plan_hook` is on. `action` says what
  Terraform intends: `create`, `update` (any attribute changed; check
  `input_changed` for the managed state itself), `delete`, or `none`. Values
  not known until apply are sent as null and listed in `unknown`. Return
  `errors` to reject the configuration before anything is applied.
  `requires_replace` turns an update into delete-and-create; it applies when
  `input` changed, or on any change when `always_update` is set, and is
  reported as a warning otherwise. `output` promises values that will be
  known after apply, so dependents can plan with them; the apply must then
  return the same values or fail.
- **`create`** must return a non-empty `id`. Returning the id of an object
  that already exists is a legitimate way to adopt it. Write `create` so that
  a half-finished one is recoverable: if the program is stopped between
  creating the object and answering, Terraform does not know the object.
- **`read`** says the object is gone with `{"exists": false}`; Terraform then
  plans to create it again. If the response has an `input` key, it replaces
  the stored desired state, which is how changes made outside Terraform become
  visible. Report only the fields Terraform manages (the keys of the request's
  `input`, which `req.managed()` does for you), with the types the
  configuration uses; a value whose type differs, such as `"6"` for `6`, is a
  real diff. When no `input` key is present the stored value is kept.
- **`update`** runs only when `input` changed, unless `always_update` is set,
  in which case `input_changed` tells which it was. After a default `move` or
  `import` (see below) `prior_input` is null: reconcile against the live
  object rather than against `prior_input`. The `id` cannot change on update;
  ask for replacement from the plan hook instead.
- **`delete`** removes the object. There is no default: to forget an object
  without deleting it, use a `removed` block or `terraform state rm`.
- **`import`** receives the import id and returns the full state. The id
  comes from the resource identity (`import { to = ..., identity = { id =
  "web-1" } }`, Terraform ≥ 1.12), from a plain `terraform import
  scripted_resource.x web-1`, or from a JSON id that also names the program
  for resources whose program is not on the provider block:
  `terraform import scripted_resource.x '{"id": "web-1", "program": ["python3", "manage.py"]}'`
  (`context` may be given the same way). The response may also set `context`,
  so that the read which follows has what it needs. The default keeps just
  the id and lets `read` fill in the rest; the first plan is then an update
  that the program should treat as a reconcile.
- **`move`** receives the source resource's provider address, type, schema
  version and raw state (`source`), and returns the full state like `import`.
  It needs the provider's program, since a moved block has no resource
  configuration yet. This is where knowledge of other providers' state
  layouts belongs: a handful of lines that pick the id, the managed values
  and the output out of `source.state`. The default carries over `id` if the
  source state has one, and the first plan is an in-place update that fills
  in `program` and `input` from configuration and runs `update` once with
  `prior_input = null`.
- **`data`** answers a `scripted_data` lookup with `output`.
- **Private state**: any response may carry `private`, arbitrary JSON that is
  stored in state out of sight (not in plans, not in outputs) and sent back
  with every later request. Use it for ETags, operation URLs to resume, or
  anything the object itself does not record. An absent key keeps the stored
  value; an explicit null clears it. See the note on `create` above.
- **Errors**: `errors` fails the operation with each message; `warnings` are
  shown. If the program exits, every request it had not answered fails with
  its exit status and the tail of its stderr; the next request starts it
  again.

### Not implemented, and staying compatible

Answer `{"not_implemented": true}` to any operation the program does not
handle, including ones it has never heard of. The provider then applies its
default: plan changes nothing, read trusts the state, import keeps the id,
move carries over the id. `create`, `update`, `delete` and `data` have no
default and fail with a clear message. The helper does this for every
operation without a handler.

That rule is what keeps old programs working as the provider grows. The
contract:

| change                                            | safe?                              | bumps `protocol`? |
|---------------------------------------------------|------------------------------------|-------------------|
| a new operation                                   | yes: answer `not_implemented`      | no                |
| a new request field                               | yes: ignore unknown fields         | no                |
| a new response field                              | yes: the provider ignores unknowns | no                |
| an existing field or operation changes meaning    | **no**                             | **yes**           |

`protocol` (also in the `SCRIPTED_PROTOCOL` environment variable) is the one
number a program author needs to know; it is not the provider version. A
program may declare the protocol it was written for in its responses; the
provider refuses to talk to one that declares a higher version than it knows.

### Concurrency and cost

Terraform runs up to `-parallelism` (default 10) operations at once, and the
provider sends them to the program as they come: several requests are
outstanding before the first is answered. With the helper, handlers run in
threads, one per request, so ten resources take the time of one; a raw
program that reads a line, answers it, and reads the next is correct too,
but serialises them. The rules that follow:

- Handlers keep to local variables; per-object state goes in `private`.
- Clients that are not thread-safe (`requests.Session`, database
  connections, cloud SDK sessions) are held through `r.local(factory)`, one
  per worker thread for the life of the process: `session =
  r.local(requests.Session)` and `session()` inside handlers. A process
  lives for one graph walk, so nothing held in memory — a token, a warmed
  connection, an `r.local()` client — outlives it; anything that must
  persist belongs in `private`.
- Handlers run on a pool of worker threads, 10 by default (Terraform's
  default parallelism; raise it alongside `-parallelism`).
  `resource(concurrent=4)` sets the number of workers, for a rate-limited
  API; `resource(concurrent=False)` handles one request at a time.
- `TF_LOG=DEBUG` attributes stderr to the program, not to a request: with
  requests handled concurrently, what is logged with an answer is whatever
  the program wrote since the previous answer.

Because the program is started once per graph walk rather than per
operation, interpreter start-up and imports cost two or three times per
command, not hundreds. What a command costs in requests per resource:

| Terraform command                 | requests per resource          |
|-----------------------------------|--------------------------------|
| `plan`, resource unchanged        | `read`, `plan`                 |
| `plan`, resource not yet created  | `plan`                         |
| `apply` that creates              | `plan` ×2, `create`            |
| `apply` that changes `input`      | `read`, `plan` ×2, `update`    |
| `destroy`                         | `read`, `plan` ×2, `delete`    |

The ×2 is Terraform planning again during the apply walk. `plan_hook = false`
removes the `plan` rows; `always_update = true` adds an `update` to every
apply that touches the resource.

### Timeouts and stopping

`timeout` bounds how long one request may take from the moment it is sent
to the moment it is answered — **including time spent waiting for the
program to get to it**, so size it for the queue as well as for one
operation when a program handles requests one at a time. It is 10 minutes
unless set on the resource or the provider. A request that is not answered
in time fails on its own, with a message saying so; a late answer to it,
however late, is discarded (the provider issues the ids, so it knows one of
its own), and the requests in flight beside it are unaffected, since every
answer names the request it belongs to. Only when the program has answered
nothing at all while three requests in a row timed out is it considered
stuck and stopped: its stdin is closed (a conforming
program exits), then after ten seconds it receives SIGTERM, then SIGKILL,
and everything still in flight fails. The same happens when Terraform is
interrupted. A program that reads stdin to its end instead of line by line
never answers and is what a timeout most often catches; one that echoes a
`request_id` that matches no request is reported as such, naming the ids.

### Trying a program without Terraform

The protocol is plain stdin/stdout, so a program can be driven by hand:

```sh
echo '{"request_id":"1","op":"read","id":"web-1","input":{"name":"web-1","size":2}}' | python3 manage.py
python3 manage.py --op read --id web-1 --input '{"name":"web-1","size":2}'   # with the helper
```

`TF_LOG=DEBUG` logs every exchange: the command, environment variable names,
exit status, duration, stderr and a summary of each response; `TF_LOG=TRACE`
adds the full request and response bodies, which include `provider_input`
and `input` and so whatever secrets those hold.

### Common mistakes

- Printing anything but responses to stdout. Use stderr; the helper redirects
  `print` for you. A stray line ends the program and fails the next request.
- Reading stdin to its end (`json.load(sys.stdin)`) instead of line by line.
- Echoing the wrong `request_id`, or none: the provider reports which ids
  were outstanding.
- A module-level `requests.Session()` or database connection shared by
  threads: hold it through `r.local()`.
- Returning fields from `read` that Terraform does not manage: they show as
  permanent drift. Return the keys of the request's `input` only.
- Returning `"6"` where the configuration has `6`.
- Forgetting the `not_implemented` answer for unknown operations (the helper
  cannot forget).
- Heavy work at import time of the program is fine (once per command); heavy
  work in `plan` or `read` runs on every plan for every resource.

### Types

`input` and `output` are dynamic: they take whatever type the configuration
or the program's JSON gives them. On `read`, the returned `input` is shaped
after the type already in state where the JSON allows it, so a `list(string)`
stays a list rather than becoming a tuple. Numbers are compared by value, so
`6`, `6.0` and a list versus a tuple with equal elements count as unchanged.
Mark individual values sensitive in configuration with `sensitive()`; the
plan then hides just those while still showing the rest field by field.

### Identity

The resource's identity, as Terraform ≥ 1.12 understands it, is its `id`:
the one value that never changes for the life of the object. It is recorded
in state and is what `import { identity = { id = ... } }` takes. `program`,
`context` and the rest cannot be part of it, since Terraform forbids an
identity change without replacement and those may change at any time.

### Moving from another resource type

```hcl
moved {
  from = script.stack
  to   = scripted_resource.stack
}
```

With a provider-level `program`, the program's `move` operation decides what
the new state is; when it adopts the object fully (returns `id`, `input` and
`output`), the plan after the move is a no-op. Otherwise the default keeps
the `id` and the first plan is an in-place update, with no call to the old
provider — it does not need to stay declared. Terraform 1.8 or later is
required for a `moved` block that changes the resource type.

## Security

- **Planning executes the program**, as said at the top. Anyone who can
  change the configuration or the program's source can run code wherever
  plans run.
- The program inherits Terraform's environment unless the provider sets
  `inherit_environment = false`, in which case it sees only `PATH`, `HOME`,
  locale variables and what `environment` sets.
- A `program` whose command has no path separator is found through `PATH`:
  `["python3", ...]` means whatever `python3` Terraform's `PATH` finds, which
  in CI is not always what you expect. A relative path is resolved against
  `working_dir`.
- `sensitive_environment` is redacted in plans but stored in state. The
  provider's `environment` and `input` never reach state and are the place
  for credentials.
- `TF_LOG=TRACE` writes requests and responses, secrets included, to the log.
