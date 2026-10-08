// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Operations with no default fail clearly when the program declines them,
// and an update may not change the id.
func TestAccResource_mandatoryOperations(t *testing.T) {
	b := newBackend(t)
	withKnob := func(knob, op string, size int) string {
		return b.providerBlock(knob, op) + fmt.Sprintf(`
resource "scripted_resource" "test" {
  input = { name = "mand", size = %d }
}
`, size)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("mand"),
		Steps: []resource.TestStep{
			{
				Config:      withKnob("BACKEND_NOTIMPL", "create", 1),
				ExpectError: regexp.MustCompile(`Create not implemented`),
			},
			{Config: withKnob("BACKEND_TAG", "x", 1)},
			{
				Config:      withKnob("BACKEND_NOTIMPL", "update", 2),
				ExpectError: regexp.MustCompile(`Update not implemented`),
			},
			{
				Config:      withKnob("BACKEND_NEWID", "update", 3),
				ExpectError: regexp.MustCompile(`Update changed the id`),
			},
			{
				Config:      withKnob("BACKEND_NOTIMPL", "delete", 1),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`Delete not implemented`),
			},
			{
				Config:      withKnob("BACKEND_FAIL", "delete", 1),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`Delete failed`),
			},
			// Back to a program that can delete, so the test's own destroy works.
			{Config: withKnob("BACKEND_TAG", "y", 1)},
		},
	})
}

// A plan hook that promises output the program then does not deliver is an
// error at apply, not a silent inconsistency.
func TestAccResource_brokenPromise(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: b.providerBlock("BACKEND_BREAK_PROMISE", "1") + `
resource "scripted_resource" "test" {
  input = { name = "promise", known_output = true }
}
`,
				ExpectError: regexp.MustCompile(`differs from what its plan hook promised`),
			},
		},
	})
}

func TestAccDataSource_notImplementedAndWarnings(t *testing.T) {
	b := newBackend(t)
	wd, _ := os.Getwd()
	minimal := filepath.Join(wd, "testdata", "minimal.py")
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
provider "scripted" {
  program = ["python3", %q]
  %s
}
data "scripted_data" "test" {
  input = { name = "x" }
}
`, minimal, b.env()),
				ExpectError: regexp.MustCompile(`Data not implemented`),
			},
			{
				// A plan response with a field that means nothing for plan is
				// accepted with a warning; the apply goes through.
				Config: b.providerBlock() + `
resource "scripted_resource" "test" {
  input = { name = "misplaced", misplaced = true }
}
`,
				Check: resource.TestCheckResourceAttr(testRes, "id", "misplaced"),
			},
		},
	})
}

// inherit_environment = false hides Terraform's own environment from the
// program; the default passes it on.
func TestAccResource_inheritEnvironment(t *testing.T) {
	t.Setenv("SCRIPTED_TEST_CANARY", "1")
	b := newBackend(t)
	cfg := func(inherit string) string {
		return fmt.Sprintf(`
provider "scripted" {
  %s
  %s
  %s
}
resource "scripted_resource" "test" {
  input = { name = "env" }
}
`, b.program(), b.env("BACKEND_ECHO_ENV", "SCRIPTED_TEST_CANARY"), inherit)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("env"),
		Steps: []resource.TestStep{
			{
				Config: cfg(""),
				Check:  resource.TestCheckResourceAttr(testRes, "output.env_present", "true"),
			},
			{
				// Taint so create runs again under the new provider setting.
				Config: cfg("inherit_environment = false"),
				Taint:  []string{testRes},
				Check:  resource.TestCheckResourceAttr(testRes, "output.env_present", "false"),
			},
		},
	})
}

func TestAccResource_invalidResourceTimeout(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: b.providerBlock() + `
resource "scripted_resource" "test" {
  timeout = "soon"
  input   = { name = "t" }
}
`,
				ExpectError: regexp.MustCompile(`Invalid timeout`),
			},
		},
	})
}

// A moved block whose program fails the move surfaces the failure on the
// read that follows.
func TestAccResource_moveFailure(t *testing.T) {
	b := newBackend(t)
	wd, _ := os.Getwd()
	shim := filepath.Join(wd, "testdata", "czmirek_backend.py")
	legacy := b.providerBlock() + fmt.Sprintf(`
resource "script" "legacy" {
  create       = ["python3", %[1]q, "create", "mvf", "{\"name\": \"mvf\"}"]
  read         = ["python3", %[1]q, "read", "mvf", "{}"]
  update       = ["python3", %[1]q, "update", "mvf", "{\"name\": \"mvf\"}"]
  delete       = ["python3", %[1]q, "delete", "mvf", "{}"]
  target_state = ["python3", %[1]q, "target_state", "mvf", "{\"name\": \"mvf\"}"]
  working_dir  = %[2]q
}
`, shim, b.dir)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		ExternalProviders: map[string]resource.ExternalProvider{
			"script": {Source: "czmirek/script", VersionConstraint: "0.1.2"},
		},
		Steps: []resource.TestStep{
			{Config: legacy},
			{
				Config: b.providerBlock("BACKEND_FAIL", "move") + `
moved {
  from = script.legacy
  to   = scripted_resource.test
}
resource "scripted_resource" "test" {
  input = { name = "mvf" }
}
`,
				ExpectError: regexp.MustCompile(`Move failed`),
			},
			{
				// With a program that can move, it goes through.
				Config: b.providerBlock() + `
moved {
  from = script.legacy
  to   = scripted_resource.test
}
resource "scripted_resource" "test" {
  input = { name = "mvf" }
}
`,
				Check: resource.TestCheckResourceAttr(testRes, "id", "mvf"),
			},
		},
	})
}
