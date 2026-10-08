// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// providerBlock configures the program and backend directory on the provider,
// so resources need neither.
func (b backend) providerBlock(extraEnv ...string) string {
	return fmt.Sprintf(`
provider "scripted" {
  %s
  %s
}
`, b.program(), b.env(extraEnv...))
}

func (b backend) lastOp(t *testing.T, op string) map[string]any {
	t.Helper()
	ops := b.ops(t)
	for i := len(ops) - 1; i >= 0; i-- {
		if ops[i]["op"] == op {
			return ops[i]
		}
	}
	return nil
}

func TestAccResource_providerLevelProgram(t *testing.T) {
	b := newBackend(t)
	cfg := func(size int) string {
		return b.providerBlock() + fmt.Sprintf(`
resource "scripted_resource" "test" {
  input = { name = "plp", size = %d }
}
`, size)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			b.checkDestroy("plp"),
			func(_ *terraform.State) error {
				// The plan hook is consulted for destroys too.
				if op := b.lastOp(t, "plan"); op == nil || op["action"] != "delete" || fmt.Sprint(op["id"]) != "plp" {
					return fmt.Errorf("expected a plan op with action delete for plp, last plan op: %v", op)
				}
				return nil
			},
		),
		Steps: []resource.TestStep{
			{
				Config: cfg(1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "id", "plp"),
					resource.TestCheckResourceAttr(testRes, "output.address", "plp.example"),
					resource.TestCheckNoResourceAttr(testRes, "program.#"),
					resource.TestCheckResourceAttr(testRes, "plan_hook", "true"),
					check(func() error {
						op := b.lastOp(t, "plan")
						if op == nil || op["action"] != "create" || fmt.Sprint(op["protocol"]) != "1" {
							return fmt.Errorf("expected a plan op with action create and protocol 1, got %v", op)
						}
						return nil
					}),
				),
			},
			{
				Config:           cfg(2),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "output.generation", "2"),
					check(func() error {
						op := b.lastOp(t, "plan")
						if op["action"] != "update" || op["input_changed"] != true {
							return fmt.Errorf("plan op should say action update with input_changed: %v", op)
						}
						return nil
					}),
				),
			},
		},
	})
}

func TestAccResource_importWithProviderProgram(t *testing.T) {
	b := newBackend(t)
	config := b.providerBlock() + `
resource "scripted_resource" "test" {
  input = { name = "imp", size = 3 }
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("imp"),
		Steps: []resource.TestStep{
			{Config: config},
			{
				Config:            config,
				ResourceName:      testRes,
				ImportState:       true,
				ImportStateId:     "imp",
				ImportStateVerify: true,
				Check: check(func() error {
					if op := b.lastOp(t, "import"); op == nil || fmt.Sprint(op["id"]) != "imp" {
						return fmt.Errorf("import op not seen: %v", op)
					}
					return nil
				}),
			},
		},
	})
}

func TestAccResource_importWithJSONID(t *testing.T) {
	b := newBackend(t)
	config := fmt.Sprintf(`
resource "scripted_resource" "test" {
  %s
  %s
  input = { name = "impj", size = 3 }
}
`, b.program(), b.env())
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("impj"),
		Steps: []resource.TestStep{
			{Config: config},
			{
				Config:            config,
				ResourceName:      testRes,
				ImportState:       true,
				ImportStateId:     fmt.Sprintf(`{"id": "impj", "program": ["python3", %q], "environment": {"BACKEND_DIR": %q}}`, b.script, b.dir),
				ImportStateVerify: true,
			},
		},
	})
}

// With a provider-level program, a moved block from czmirek/script is handed
// to the program's move operation, which adopts the object completely: the
// plan after the move is a no-op.
func TestAccResource_moveViaProgram(t *testing.T) {
	b := newBackend(t)
	wd, _ := os.Getwd()
	shim := filepath.Join(wd, "testdata", "czmirek_backend.py")
	data := `jsonencode({ name = "mvp", size = 1 })`
	legacy := b.providerBlock() + fmt.Sprintf(`
resource "script" "legacy" {
  create       = ["python3", %[1]q, "create", "mvp", %[2]s]
  read         = ["python3", %[1]q, "read", "mvp", "{}"]
  update       = ["python3", %[1]q, "update", "mvp", %[2]s]
  delete       = ["python3", %[1]q, "delete", "mvp", "{}"]
  target_state = ["python3", %[1]q, "target_state", "mvp", %[2]s]
  working_dir  = %[3]q
}
`, shim, data, b.dir)
	moved := b.providerBlock() + `
moved {
  from = script.legacy
  to   = scripted_resource.test
}

resource "scripted_resource" "test" {
  context = { adopted = true }
  input   = { name = "mvp", size = 1 }
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		ExternalProviders: map[string]resource.ExternalProvider{
			"script": {Source: "czmirek/script", VersionConstraint: "0.1.2"},
		},
		CheckDestroy: b.checkDestroy("mvp"),
		Steps: []resource.TestStep{
			{Config: legacy},
			{
				Config:           moved,
				ConfigPlanChecks: expectAction(plancheck.ResourceActionNoop),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "id", "mvp"),
					resource.TestCheckResourceAttr(testRes, "input.size", "1"),
					resource.TestCheckResourceAttr(testRes, "context.adopted", "true"),
					resource.TestCheckResourceAttr(testRes, "output.generation", "1"),
					expectOps(t, b, map[string]int{"move": 1, "create": 0, "update": 0}),
					check(func() error {
						op := b.lastOp(t, "move")
						src, _ := op["source"].(map[string]any)
						if src == nil || src["type"] != "script" || fmt.Sprint(src["provider"]) != "registry.terraform.io/czmirek/script" {
							return fmt.Errorf("move op should describe the source: %v", op)
						}
						state, _ := src["state"].(map[string]any)
						if fmt.Sprint(state["id"]) != "mvp" {
							return fmt.Errorf("move op should carry the raw source state: %v", op)
						}
						return nil
					}),
				),
			},
		},
	})
}

// The plan hook rejects bad configuration before anything is applied, and
// can be turned off per resource.
func TestAccResource_planHookErrors(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      cfg(b, "neg", -1),
				ExpectError: regexp.MustCompile(`Program error \(plan\)[\s\S]*size must be >= 0`),
			},
			{
				Config: b.providerBlock() + `
resource "scripted_resource" "test" {
  plan_hook = false
  input     = { name = "neg", size = -3 }
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "input.size", "-3"),
					check(func() error {
						for _, op := range b.ops(t) {
							in, _ := op["input"].(map[string]any)
							if op["op"] == "plan" && fmt.Sprint(in["size"]) == "-3" {
								return fmt.Errorf("plan hook ran although plan_hook = false: %v", op)
							}
						}
						return nil
					}),
				),
			},
		},
	})
}

func TestAccResource_planHookKnownOutput(t *testing.T) {
	b := newBackend(t)
	config := b.providerBlock() + `
resource "scripted_resource" "test" {
  input = { name = "ko", known_output = true }
}

output "address_at_plan" {
  value = scripted_resource.test.output.address
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("ko"),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectKnownValue(testRes, tfjsonpath.New("output").AtMapKey("address"), knownvalue.StringExact("ko.example")),
						plancheck.ExpectKnownOutputValue("address_at_plan", knownvalue.StringExact("ko.example")),
					},
				},
				Check: resource.TestCheckResourceAttr(testRes, "output.generation", "1"),
			},
		},
	})
}

func TestAccResource_alwaysUpdate(t *testing.T) {
	b := newBackend(t)
	withTag := func(tag string) string {
		return fmt.Sprintf(`
resource "scripted_resource" "test" {
  %s
  %s
  always_update = true
  input = { name = "au" }
}
`, b.program(), b.env("BACKEND_TAG", tag))
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("au"),
		Steps: []resource.TestStep{
			{
				Config: withTag("a"),
				Check:  resource.TestCheckResourceAttr(testRes, "output.tag", "a"),
			},
			{
				Config:           withTag("b"),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "output.tag", "b"),
					resource.TestCheckResourceAttr(testRes, "output.generation", "2"),
					expectOps(t, b, map[string]int{"update": 1}),
					check(func() error {
						if op := b.lastOp(t, "update"); op["input_changed"] != false {
							return fmt.Errorf("update should report input_changed = false: %v", op)
						}
						return nil
					}),
				),
			},
		},
	})
}

// The program keeps a counter in private state across reads; nothing of it
// appears in the plan.
func TestAccResource_privateState(t *testing.T) {
	b := newBackend(t)
	config := b.providerBlock() + `
resource "scripted_resource" "test" {
  input = { name = "priv" }
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("priv"),
		Steps: []resource.TestStep{
			{Config: config},
			// The read during this step's refresh sees the counter create
			// stored (0) and stores 1; the apply persists that.
			{Config: config},
			{
				Config: config,
				Check: resource.TestCheckResourceAttrWith(testRes, "output.reads_seen", func(v string) error {
					n, err := strconv.Atoi(v)
					if err != nil || n < 1 {
						return fmt.Errorf("reads_seen should count earlier reads, got %q", v)
					}
					return nil
				}),
			},
		},
	})
}

// A program that implements only create/read/update/delete and answers
// not_implemented to everything else gets the default behaviour everywhere.
func TestAccResource_notImplementedDefaults(t *testing.T) {
	b := newBackend(t)
	wd, _ := os.Getwd()
	minimal := filepath.Join(wd, "testdata", "minimal.py")
	config := fmt.Sprintf(`
provider "scripted" {
  program = ["python3", %q]
  %s
}

resource "scripted_resource" "test" {
  input = { name = "min", size = 1 }
}
`, minimal, b.env())
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("min"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "output.address", "min.example"),
					// plan was asked and declined
					expectOps(t, b, map[string]int{"create": 1}),
					check(func() error {
						if b.count(t, "plan") == 0 {
							return fmt.Errorf("optional ops should still be offered: %v", b.opNames(t))
						}
						return nil
					}),
				),
			},
			{
				// Default import: id only, then read fills in input and output.
				Config:            config,
				ResourceName:      testRes,
				ImportState:       true,
				ImportStateId:     "min",
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccResource_noProgramAnywhere(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "scripted_resource" "test" {
  input = { name = "nothing" }
}
`,
				ExpectError: regexp.MustCompile(`No program to run`),
			},
		},
	})
}
