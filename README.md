# Terraform Provider: scripted

Manage anything with a script, without giving up Terraform's lifecycle.

`scripted_resource` hands each operation (`plan`, `create`, `read`, `update`,
`delete`, `import`, `move`) to a program you write and records what it
answers. The desired state lives in a structured `input` attribute that the
program refreshes on `read`, so you get drift detection, field-level plans,
replacement on immutable fields, and no-op applies when nothing really
changed — the things a generic "run this shell command" provider cannot do.
The program answers `not_implemented` to any operation it leaves to the
provider's defaults, which is also what keeps old programs working when new
operations are added. `scripted_data` runs the same kind of program
read-only.

The program is started once per provider instance (Terraform starts one per
graph walk, so two or three times in an `apply`) and streams requests, one
JSON document per line, so interpreter start-up is paid a few times rather
than per operation.

```hcl
provider "scripted" {
  # Used by every resource that sets no program of its own, and by
  # `terraform import` and `moved` blocks. Never stored in state.
  program = ["python3", "${path.root}/manage.py"]
  input   = { store_dir = "${path.root}/store" } # sent as provider_input
}

resource "scripted_resource" "record" {
  input = {
    name = "web-1"
    kind = "vm"
    size = 2
  }
}
```

The program, with the vendored helper (`terraform-provider-scripted
--emit-helper > scripted.py`):

```python
from scripted import resource
r = resource()

@r.create
def create(req):
    rec = api.create(req.input)
    return r.ok(id=rec["id"], output={"addr": rec["addr"]})

@r.read
def read(req):
    rec = api.get(req.id)
    if rec is None:
        return r.gone()
    return r.ok(input=req.managed(rec["spec"]), output={"addr": rec["addr"]})

@r.update
def update(req):
    return r.ok(output={"addr": api.update(req.id, req.input)["addr"]})

@r.delete
def delete(req):
    api.delete(req.id)

r.main()
```

See [docs/resources/resource.md](docs/resources/resource.md) for the protocol
and complete example programs, [docs/data-sources/data.md](docs/data-sources/data.md)
for the data source, and [docs/index.md](docs/index.md) for the provider.

## Security

**`terraform plan` executes your program.** `plan` and `read` run during a
plan, with Terraform's environment available to the program unless the
provider sets `inherit_environment = false`. In a plan-on-pull-request setup,
a pull request that changes the program runs that code. Treat `program` like
any other code your CI executes. Credentials go in the provider's
`environment` or `input`, which never reach state. See [SECURITY.md](SECURITY.md).

## Status

Prototype. The protocol may still change before 1.0; see the compatibility
contract in the resource documentation for what will and will not change
without notice.

## Requirements

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.8
  (1.8 for `moved` blocks that change the resource type, 1.12 for import by
  identity; the resource itself works on any 1.x that speaks protocol 6)
- [Go](https://golang.org/doc/install) >= 1.26 to build
- Tested on Linux. macOS should behave the same. Windows binaries are built
  but untested: `["python3", "manage.py"]` and shebang lines work differently
  there.

## The helper

`helpers/python/scripted.py` is a standard-library-only file meant to be
copied next to your program (`terraform-provider-scripted
--emit-helper > scripted.py` prints the copy matching your provider, with its
version stamped in). It gives you handlers as plain functions, the
`not_implemented` rule for free, exceptions turned into errors, stdout kept
clean, and `python3 manage.py --op read --id x` for trying a program without
Terraform.

**Handlers run concurrently by default**, on a pool of worker threads, so
ten resources take the time of one. Keep handlers to local variables, put
per-object state in `private`, and hold clients that are not thread-safe
(`requests.Session`, database connections) through `r.local(factory)`, which
builds one per worker and keeps it for the life of the process (one graph
walk; anything that must persist goes in `private`). `resource(concurrent=4)` sets the number
of workers; `resource(concurrent=False)` handles one request at a time (note
that a request's `timeout` then also covers its wait in the queue).

## What is delegated to the program

| Terraform concept                        | status                                              |
|------------------------------------------|-----------------------------------------------------|
| plan (replace, promised output, errors)  | yes, `plan`                                         |
| create / read / update / delete          | yes                                                 |
| import                                   | yes, `import` (identity, plain or JSON id)          |
| moved from another resource type         | yes, `move` (Terraform ≥ 1.8)                       |
| private state                            | yes, `private`                                      |
| data source                              | yes, `scripted_data`                                |
| resource identity (Terraform ≥ 1.12)     | yes: the `id`                                       |
| ephemeral resources (Terraform ≥ 1.10)   | planned                                             |
| write-only attributes (Terraform ≥ 1.11) | planned, for secrets in `input`                     |
| provider-defined functions, actions      | no                                                  |

## Development

```sh
make build        # go build
make test         # unit tests
make testacc      # acceptance tests; needs terraform and python3 in PATH
make lint         # golangci-lint
make generate     # terraform fmt the examples, copy the helper, regenerate docs/
```

To use a local build from a Terraform project, point a
[dev override](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers)
at the directory holding the binary:

```hcl
# ~/.terraformrc
provider_installation {
  dev_overrides {
    "audiocodes/scripted" = "/home/you/go/bin"
  }
  direct {}
}
```

The acceptance tests exercise the full lifecycle against a fake backend
(`internal/provider/testdata/backend.py`): drift, metadata-only changes,
replacement and promised output via the plan hook, plan-time errors,
`always_update` with replacement, timeouts, stray output, concurrency,
invocation counts, private state, import, the state move from
`czmirek/script` (program-handled and default), the data source, a minimal
raw-protocol program (`testdata/minimal.py`) and the memory example.

## Releasing

A release is a pull request that adds a version heading to the top of
`CHANGELOG.md`, such as `## 1.2.0`, with the changes under it. When it
merges, the Release workflow runs the tests, tags the commit `v1.2.0`, builds
and signs it with [GoReleaser](https://goreleaser.com) in the layout the
Terraform Registry expects, and publishes the GitHub release with that
section as its notes; the Registry picks it up from there. Merges that add
no new version release nothing.

The workflow needs two repository secrets: `GPG_PRIVATE_KEY`
(ASCII-armoured) and `PASSPHRASE`, for the key whose public part is
registered with the Registry. A published version is never re-released:
fix forward with a new version.

## License

[Apache License 2.0](LICENSE), provider and helper alike; the helper may be
vendored into any project.
