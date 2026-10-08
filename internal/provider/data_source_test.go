// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func TestAccDataSource_basic(t *testing.T) {
	b := newBackend(t)
	config := b.providerBlock() + `
resource "scripted_resource" "test" {
  input = { name = "ds" }
}

data "scripted_data" "found" {
  input = { name = scripted_resource.test.id }
}

data "scripted_data" "missing" {
  input = { name = "nobody", extra = [1, 2] }
}

data "scripted_data" "own_program" {
  ` + b.program() + `
  ` + b.env("BACKEND_TAG", "x") + `
  input = { name = "nobody" }
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("ds"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.scripted_data.found", "output.found", "true"),
					resource.TestCheckResourceAttr("data.scripted_data.found", "output.generation", "1"),
					resource.TestCheckResourceAttr("data.scripted_data.found", "output.echo.name", "ds"),
					resource.TestCheckResourceAttr("data.scripted_data.missing", "output.found", "false"),
					resource.TestCheckResourceAttr("data.scripted_data.missing", "output.echo.extra.1", "2"),
					resource.TestCheckResourceAttr("data.scripted_data.own_program", "output.found", "false"),
					check(func() error {
						if n := b.count(t, "data"); n < 3 {
							return fmt.Errorf("expected at least 3 data ops, got %d", n)
						}
						return nil
					}),
				),
			},
		},
	})
}

func TestAccDataSource_failures(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: b.providerBlock("BACKEND_FAIL", "data") + `
data "scripted_data" "test" {
  input = { name = "x" }
}
`,
				ExpectError: regexp.MustCompile(`data: the program exited \(exit status 2\)[\s\S]*forced failure of data`),
			},
			{
				Config: `
data "scripted_data" "test" {
  input = { name = "x" }
}
`,
				ExpectError: regexp.MustCompile(`No program to run`),
			},
		},
	})
}
