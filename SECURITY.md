# Security

## What this provider does

It runs a program you point it at. `terraform plan` is enough to run it:
the `plan` and `read` operations execute during planning, with Terraform's
environment available to the program unless the provider block sets
`inherit_environment = false`. Treat the `program` attribute and the files it
points at as code your CI executes, because that is what they are.

Credentials belong in the provider block's `environment` or `input`, which
are never written to state. `sensitive_environment` on a resource is redacted
in plans but stored in state. `TF_LOG=TRACE` logs full requests and responses,
secrets included.

## Reporting a vulnerability

Please report security issues privately through GitHub's
[private vulnerability reporting](https://github.com/audiocodes/terraform-provider-scripted/security/advisories/new)
rather than as a public issue. You should hear back within a week.

## Supported versions

Only the latest release receives fixes.
