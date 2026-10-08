// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// Provider-level input and resource-level context reach the program with
// every call; neither is managed state, so changing context is a
// metadata-only update and provider input appears nowhere in state.
func TestAccResource_providerInputAndContext(t *testing.T) {
	b := newBackend(t)
	cfg := func(ctxVal int) string {
		return fmt.Sprintf(`
provider "scripted" {
  %s
  %s
  input = {
    server   = "https://stackmgr.example"
    password = sensitive("hunter2")
    retries  = 3
  }
}

resource "scripted_resource" "test" {
  context = { stack = "ctx", n = %d }
  input   = { name = "ctx" }
}

data "scripted_data" "probe" {
  context = { probe = true }
  input   = { name = "ctx" }
  depends_on = [scripted_resource.test]
}
`, b.program(), b.env(), ctxVal)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("ctx"),
		Steps: []resource.TestStep{
			{
				Config: cfg(1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "context.n", "1"),
					resource.TestCheckResourceAttr("data.scripted_data.probe", "output.provider_input.server", "https://stackmgr.example"),
					resource.TestCheckResourceAttr("data.scripted_data.probe", "output.provider_input.retries", "3"),
					resource.TestCheckResourceAttr("data.scripted_data.probe", "output.context.probe", "true"),
					check(func() error {
						op := b.lastOp(t, "create")
						pi, _ := op["provider_input"].(map[string]any)
						c, _ := op["context"].(map[string]any)
						if fmt.Sprint(pi["password"]) != "hunter2" || fmt.Sprint(c["stack"]) != "ctx" || fmt.Sprint(c["n"]) != "1" {
							return fmt.Errorf("create should carry provider_input and context: %v", op)
						}
						return nil
					}),
				),
			},
			{
				Config:           cfg(2),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "context.n", "2"),
					expectOps(t, b, map[string]int{"update": 0}),
				),
			},
			{
				// The refresh before the previous apply still carried the old
				// context; from now on reads see the new one.
				Config: cfg(2),
				Check: resource.ComposeAggregateTestCheckFunc(
					expectOps(t, b, map[string]int{"update": 0}),
					check(func() error {
						c, _ := b.lastOp(t, "read")["context"].(map[string]any)
						if fmt.Sprint(c["n"]) != "2" {
							return fmt.Errorf("read should see the new context: %v", c)
						}
						return nil
					}),
				),
			},
		},
	})
}
