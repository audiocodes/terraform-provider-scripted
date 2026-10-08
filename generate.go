// SPDX-License-Identifier: Apache-2.0

package main

// Keep the vendored helper in the examples identical to the embedded one,
// format the Terraform in the examples, then render docs/ from the schema,
// the templates/ directory and those examples.
//
//go:generate cp helpers/python/scripted.py examples/resources/scripted_resource/scripted.py
//go:generate terraform fmt -recursive ./examples/
//go:generate go tool tfplugindocs generate -provider-name scripted
