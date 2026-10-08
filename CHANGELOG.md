# Changelog

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
