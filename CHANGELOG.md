# Changelog

## 0.3.0

- The Python helper no longer needs copying into your project. The provider
  writes the copy matching its version to
  `.terraform/scripted/python/scripted.py` (under `$TF_DATA_DIR` when set) as
  soon as Terraform loads it, so on `terraform validate` or any plan, and
  appends that directory to every program's `PYTHONPATH`;
  `SCRIPTED_PYTHON_HELPER` holds the file's path. A copy next to the program
  still takes precedence. If the data directory cannot be written, the user
  cache directory and then the system temporary directory are used.
- `--emit-helper` is now `--emit-python-helper`, leaving room for helpers in
  other languages.
- The variables the provider injects (`SCRIPTED_PROTOCOL`, `PYTHONPATH`,
  `SCRIPTED_PYTHON_HELPER`) are logged, at debug level, when a program
  starts.
- On Windows, a program that has to be stopped is killed at once instead of
  after a grace period spent waiting for a SIGTERM that cannot be sent.
- Built with Go 1.26.9 and golang.org/x/net v0.60.0, for the HTTP/2 and
  net/http advisories GO-2026-6608, -6611, -6612, -6613 and -6617.

## 0.2.0

- `timeout = "0"`, on the provider or a resource, removes the limit on
  requests, for programs that bound their own operations. Unset still
  means 10 minutes.

## 0.1.0 (2026-10-08)

Initial release.

- `scripted_resource`: delegate a resource's lifecycle to a program, with a
  refreshable structured `input`, update-only-on-change semantics (or
  `always_update`), per-resource `context`, and opaque private state.
- One program process per provider instance (per Terraform graph walk):
  requests stream to it one JSON document per line with a `request_id`,
  answers come back in any order. A
  request that times out (10 minutes by default) fails on its own; the
  program is stopped only when it stops answering altogether.
- Program, working directory, environment, structured `input`, timeout and
  environment inheritance configurable on the provider, with resource-level
  overrides where they make sense.
- Optional operations `plan` (replacement, promised output, diagnostics,
  destroy awareness), `import` and `move`, each with a default behaviour when
  the program answers `not_implemented`; `moved` blocks hand the program the
  raw source state.
- Resource identity (`id`), for `import { identity = ... }` on Terraform ≥ 1.12.
- `scripted_data` data source.
- `scripted.py`, a vendorable, standard-library-only helper for programs in
  Python, emitted by `terraform-provider-scripted --emit-helper`; handlers
  run concurrently in threads by default, with `r.local()` for clients that
  are not thread-safe.
