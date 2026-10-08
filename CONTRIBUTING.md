# Contributing

Thanks for taking an interest. A few things that make changes easy to take:

## Setting up

- Go (see `go.mod` for the version), Terraform ≥ 1.8 and `python3` on `PATH`.
- `make build` builds, `make test` runs the unit tests, `make testacc` the
  acceptance tests (they drive real Terraform against the fake backend in
  `internal/provider/testdata`), `make lint` runs golangci-lint, and
  `make generate` refreshes `docs/` and the vendored helper copy in
  `examples/`. CI fails if `make generate` produces a diff, so run it before
  pushing.

## Changing the protocol

The request and response formats are documented in
`templates/resources/resource.md.tmpl`; the documentation is the contract.
Adding an operation or a field is compatible and needs no version change,
as long as the provider keeps treating unknown response fields as ignorable
and the docs tell programs to answer `not_implemented` to unknown operations.
Changing what an existing field or operation means bumps `protocolVersion`
in `internal/provider/exec.go` and `PROTOCOL` in `helpers/python/scripted.py`,
and needs a very good reason.

Every protocol change needs: an acceptance test, the `backend.py` fixture
taught about it, the helper updated if it affects script authors, and the
docs.

## Pull requests

- One topic per pull request.
- Tests for behaviour, not for lines.
- Commit messages say why, not just what.
