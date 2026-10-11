// SPDX-License-Identifier: Apache-2.0

package main

// Format the Terraform in the examples, then render docs/ from the schema,
// the templates/ directory and those examples.
//
//go:generate terraform fmt -recursive ./examples/
//go:generate go tool tfplugindocs generate -provider-name scripted
