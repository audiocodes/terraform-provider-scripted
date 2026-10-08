// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// The memory example remembers a value across applies, ignores a null
// new_value, and promises the result at plan time.
func TestAccResource_memoryExample(t *testing.T) {
	wd, _ := os.Getwd()
	script := filepath.Join(wd, "..", "..", "examples", "resources", "scripted_resource", "memory.py")
	cfg := func(newValue string) string {
		return fmt.Sprintf(`
resource "scripted_resource" "test" {
  program = ["python3", %q]
  input   = { new_value = %s }
}

output "remembered" {
  value = scripted_resource.test.output.value
}
`, script, newValue)
	}
	known := func(v string) resource.ConfigPlanChecks {
		return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectKnownValue(testRes, tfjsonpath.New("output").AtMapKey("value"), knownvalue.StringExact(v)),
			plancheck.ExpectKnownOutputValue("remembered", knownvalue.StringExact(v)),
		}}
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:           cfg(`"v1"`),
				ConfigPlanChecks: known("v1"),
				Check:            resource.TestCheckResourceAttr(testRes, "output.value", "v1"),
			},
			{
				// null keeps the remembered value, and the plan already knows it
				Config:           cfg("null"),
				ConfigPlanChecks: known("v1"),
				Check:            resource.TestCheckResourceAttr(testRes, "output.value", "v1"),
			},
			{
				Config:   cfg("null"),
				PlanOnly: true,
			},
			{
				Config:           cfg(`"v2"`),
				ConfigPlanChecks: known("v2"),
				Check:            resource.TestCheckResourceAttr(testRes, "output.value", "v2"),
			},
		},
	})
}
