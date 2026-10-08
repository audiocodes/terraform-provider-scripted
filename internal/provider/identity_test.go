// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// The resource has an identity (its id), recorded in state and usable for
// `import { identity = { id = ... } }`.
func TestAccResource_identity(t *testing.T) {
	b := newBackend(t)
	config := b.providerBlock() + `
resource "scripted_resource" "test" {
  input = { name = "ident", size = 1 }
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		TerraformVersionChecks: []tfversion.TerraformVersionCheck{
			tfversion.SkipBelow(tfversion.Version1_12_0),
		},
		CheckDestroy: b.checkDestroy("ident"),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectIdentity(testRes, map[string]knownvalue.Check{
						"id": knownvalue.StringExact("ident"),
					}),
				},
			},
			{
				// Import by identity through an import block (plugin-testing
				// cannot verify state for plannable imports; the next step
				// shows the imported state is coherent).
				Config:          config,
				ResourceName:    testRes,
				ImportState:     true,
				ImportStateKind: resource.ImportBlockWithResourceIdentity,
			},
			{
				// Identity survives an update unchanged.
				Config: b.providerBlock() + `
resource "scripted_resource" "test" {
  input = { name = "ident", size = 2 }
}
`,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectIdentity(testRes, map[string]knownvalue.Check{
						"id": knownvalue.StringExact("ident"),
					}),
					statecheck.ExpectKnownValue(testRes, tfjsonpath.New("output").AtMapKey("generation"), knownvalue.Int64Exact(2)),
				},
			},
		},
	})
}
