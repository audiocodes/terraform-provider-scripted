// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const testRes = "scripted_resource.test"

// cfg renders a scripted_resource "test" driving backend b.
func cfg(b backend, name string, size int) string {
	return fmt.Sprintf(`
resource "scripted_resource" "test" {
  %s
  %s
  input = {
    name  = %q
    size  = %d
    ports = [22, 80]
    tags  = tolist(["a", "b"])
  }
}
`, b.program(), b.env(), name, size)
}

func expectAction(action plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(testRes, action)},
	}
}

// check adapts a plain func to a TestCheckFunc for assertions against the
// backend rather than Terraform state.
func check(f func() error) resource.TestCheckFunc {
	return func(_ *terraform.State) error { return f() }
}

func expectOps(t *testing.T, b backend, want map[string]int) resource.TestCheckFunc {
	t.Helper()
	return check(func() error {
		for op, n := range want {
			if got := b.count(t, op); got != n {
				return fmt.Errorf("expected %d %q ops, got %d (all ops: %v)", n, op, got, b.opNames(t))
			}
		}
		return nil
	})
}

func TestAccResource_lifecycle(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("alpha"),
		Steps: []resource.TestStep{
			{
				Config:           cfg(b, "alpha", 1),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionCreate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "id", "alpha"),
					resource.TestCheckResourceAttr(testRes, "output.address", "alpha.example"),
					resource.TestCheckResourceAttr(testRes, "output.generation", "1"),
					resource.TestCheckResourceAttr(testRes, "input.size", "1"),
					resource.TestCheckResourceAttr(testRes, "plan_hook", "true"),
					expectOps(t, b, map[string]int{"create": 1, "update": 0}),
				),
			},
			{
				// A refresh must not turn the list into a tuple or otherwise
				// disturb the types that came from config.
				Config:   cfg(b, "alpha", 1),
				PlanOnly: true,
			},
			{
				Config:           cfg(b, "alpha", 2),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "id", "alpha"),
					resource.TestCheckResourceAttr(testRes, "output.generation", "2"),
					resource.TestCheckResourceAttr(testRes, "input.size", "2"),
					expectOps(t, b, map[string]int{"create": 1, "update": 1}),
					check(func() error {
						ops := b.ops(t)
						last := ops[len(ops)-1]
						if last["op"] != "update" || fmt.Sprint(last["id"]) != "alpha" {
							return fmt.Errorf("last op should be update of alpha: %v", last)
						}
						prior, _ := last["prior_input"].(map[string]any)
						if fmt.Sprint(prior["size"]) != "1" {
							return fmt.Errorf("update should see prior_input.size = 1: %v", last["prior_input"])
						}
						return nil
					}),
				),
			},
		},
	})
}

// A change made behind Terraform's back shows up as drift, and once config
// is brought in line the plan is clean without the program running update.
func TestAccResource_driftThenAdopt(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("drift"),
		Steps: []resource.TestStep{
			{
				Config: cfg(b, "drift", 1),
			},
			{
				PreConfig: func() {
					obj := b.object(t, "drift")
					in, _ := obj["input"].(map[string]any)
					in["size"] = 6
					b.writeObject(t, "drift", obj)
				},
				Config:             cfg(b, "drift", 1),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Plan-only steps never persist the refreshed state, so pick
				// up the external change explicitly (as a real plan would).
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config:   cfg(b, "drift", 6),
				PlanOnly: true,
			},
			{
				Config: cfg(b, "drift", 6),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "input.size", "6"),
					resource.TestCheckResourceAttr(testRes, "output.generation", "1"),
					expectOps(t, b, map[string]int{"create": 1, "update": 0}),
				),
			},
		},
	})
}

// Changing the program's environment is planned as an update but does not
// run the program; output stays as it was. A real input change afterwards
// runs it with the new environment.
func TestAccResource_metadataOnlyChange(t *testing.T) {
	b := newBackend(t)
	withTag := func(tag string, size int) string {
		return fmt.Sprintf(`
resource "scripted_resource" "test" {
  %s
  %s
  input = { name = "meta", size = %d }
}
`, b.program(), b.env("BACKEND_TAG", tag), size)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("meta"),
		Steps: []resource.TestStep{
			{
				Config: withTag("a", 1),
				Check:  resource.TestCheckResourceAttr(testRes, "output.tag", "a"),
			},
			{
				Config:           withTag("b", 1),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "output.tag", "a"),
					resource.TestCheckResourceAttr(testRes, "environment.BACKEND_TAG", "b"),
					expectOps(t, b, map[string]int{"create": 1, "update": 0}),
				),
			},
			{
				// The refresh after the previous apply already read with the
				// new environment, which is where output.tag changes.
				Config: withTag("b", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "output.tag", "b"),
					resource.TestCheckResourceAttr(testRes, "output.generation", "1"),
					expectOps(t, b, map[string]int{"create": 1, "update": 0}),
				),
			},
			{
				Config:           withTag("b", 2),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "output.tag", "b"),
					resource.TestCheckResourceAttr(testRes, "output.generation", "2"),
					expectOps(t, b, map[string]int{"create": 1, "update": 1}),
				),
			},
		},
	})
}

func TestAccResource_recreatedWhenGone(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("gone"),
		Steps: []resource.TestStep{
			{
				Config: cfg(b, "gone", 1),
			},
			{
				PreConfig: func() {
					if err := os.Remove(filepath.Join(b.dir, "gone.json")); err != nil {
						t.Fatal(err)
					}
				},
				Config:           cfg(b, "gone", 1),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionCreate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "output.generation", "1"),
					expectOps(t, b, map[string]int{"create": 2, "delete": 0}),
					check(func() error {
						if !b.exists("gone") {
							return fmt.Errorf("object was not recreated")
						}
						return nil
					}),
				),
			},
		},
	})
}

func TestAccResource_planHookRequiresReplace(t *testing.T) {
	b := newBackend(t)
	withImmutable := func(v string) string {
		return fmt.Sprintf(`
resource "scripted_resource" "test" {
  %s
  %s
  plan_hook = true
  input = { name = "hook", immutable = %q }
}
`, b.program(), b.env(), v)
	}
	// terraform apply plans again before applying, so the hook runs more
	// than once per step; only the relative count is meaningful.
	plansAfterCreate := 0
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("hook"),
		Steps: []resource.TestStep{
			{
				Config: withImmutable("x"),
				Check: resource.ComposeAggregateTestCheckFunc(
					expectOps(t, b, map[string]int{"create": 1}),
					check(func() error {
						plansAfterCreate = b.count(t, "plan")
						if plansAfterCreate == 0 {
							return fmt.Errorf("plan hook did not run on create")
						}
						return nil
					}),
				),
			},
			{
				Config:   withImmutable("x"),
				PlanOnly: true,
				// No input change, so the hook is not consulted.
				Check: check(func() error {
					if n := b.count(t, "plan"); n != plansAfterCreate {
						return fmt.Errorf("plan hook ran on an unchanged input (%d -> %d)", plansAfterCreate, n)
					}
					return nil
				}),
			},
			{
				Config:           withImmutable("y"),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionReplace),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "input.immutable", "y"),
					resource.TestCheckResourceAttr(testRes, "output.generation", "1"),
					expectOps(t, b, map[string]int{"create": 2, "delete": 1, "update": 0}),
				),
			},
		},
	})
}

// With values not known until apply, the plan hook still runs and is told
// which paths are unknown.
func TestAccResource_planHookUnknownInput(t *testing.T) {
	b := newBackend(t)
	config := fmt.Sprintf(`
resource "scripted_resource" "up" {
  %[1]s
  %[2]s
  input = { name = "up" }
}

resource "scripted_resource" "test" {
  %[1]s
  %[2]s
  plan_hook = true
  input = {
    name     = "down"
    upstream = scripted_resource.up.output.address
    nested   = { addr = scripted_resource.up.output.address, fixed = 1 }
  }
}
`, b.program(), b.env())
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("up", "down"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "input.upstream", "up.example"),
					check(func() error {
						for _, op := range b.ops(t) {
							if op["op"] != "plan" {
								continue
							}
							unknown := fmt.Sprint(op["unknown"])
							if unknown == "[upstream nested.addr]" || unknown == "[nested.addr upstream]" {
								return nil
							}
						}
						return fmt.Errorf("no plan op reported upstream and nested.addr as unknown: %v", b.ops(t))
					}),
				),
			},
		},
	})
}

func TestAccResource_programFailures(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Non-zero exit with a message on stderr.
				Config: fmt.Sprintf(`
resource "scripted_resource" "test" {
  %s
  %s
  input = { name = "fail" }
}
`, b.program(), b.env("BACKEND_FAIL", "create")),
				ExpectError: regexp.MustCompile(`create: the program exited \(exit status 2\)[\s\S]*forced failure of create`),
			},
			{
				// A failure reported in the response.
				PreConfig: func() {
					b.writeObject(t, "dupe", map[string]any{"input": map[string]any{}, "generation": 1})
				},
				Config:      cfg(b, "dupe", 1),
				ExpectError: regexp.MustCompile(`Program error \(create\)[\s\S]*object 'dupe' already exists`),
			},
			{
				Config: `
resource "scripted_resource" "test" {
  program = ["/nonexistent/program"]
  input   = { name = "x" }
}
`,
				ExpectError: regexp.MustCompile(`starting "/nonexistent/program"`),
			},
		},
	})
}

func TestAccResource_providerEnvironment(t *testing.T) {
	b := newBackend(t)
	config := fmt.Sprintf(`
provider "scripted" {
  environment = {
    BACKEND_DIR = %q
    BACKEND_TAG = "provider"
  }
}

resource "scripted_resource" "test" {
  %[2]s
  input = { name = "penv" }
}

resource "scripted_resource" "override" {
  %[2]s
  environment = { BACKEND_TAG = "resource" }
  input = { name = "renv" }
}
`, b.dir, b.program())
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("penv", "renv"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "output.tag", "provider"),
					resource.TestCheckResourceAttr("scripted_resource.override", "output.tag", "resource"),
					resource.TestCheckNoResourceAttr(testRes, "environment.BACKEND_DIR"),
				),
			},
		},
	})
}

// State created under czmirek/script moves over with a `moved` block even
// when no program is available to the mover (resource-level program only):
// the id carries across, the first plan is an in-place update that fills in
// program and input and runs update once, and after that the plan is clean.
func TestAccResource_moveFromCzmirekScript(t *testing.T) {
	b := newBackend(t)
	wd, _ := os.Getwd()
	shim := filepath.Join(wd, "testdata", "czmirek_backend.py")
	data := `jsonencode({ name = "mv", size = 1 })`
	legacy := fmt.Sprintf(`
resource "script" "legacy" {
  create       = ["python3", %[1]q, "create", "mv", %[2]s]
  read         = ["python3", %[1]q, "read", "mv", "{}"]
  update       = ["python3", %[1]q, "update", "mv", %[2]s]
  delete       = ["python3", %[1]q, "delete", "mv", "{}"]
  target_state = ["python3", %[1]q, "target_state", "mv", %[2]s]
  working_dir  = %[3]q
}
`, shim, data, b.dir)
	moved := fmt.Sprintf(`
moved {
  from = script.legacy
  to   = scripted_resource.test
}

resource "scripted_resource" "test" {
  %s
  %s
  input = { name = "mv", size = 1 }
}
`, b.program(), b.env())

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		ExternalProviders: map[string]resource.ExternalProvider{
			"script": {Source: "czmirek/script", VersionConstraint: "0.1.2"},
		},
		CheckDestroy: b.checkDestroy("mv"),
		Steps: []resource.TestStep{
			{
				Config: legacy,
				Check:  resource.TestCheckResourceAttr("script.legacy", "id", "mv"),
			},
			{
				Config:           moved,
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "id", "mv"),
					resource.TestCheckResourceAttr(testRes, "input.size", "1"),
					resource.TestCheckResourceAttr(testRes, "output.generation", "2"),
					expectOps(t, b, map[string]int{"create": 0, "update": 1, "delete": 0}),
					check(func() error {
						for _, op := range b.ops(t) {
							if op["op"] == "update" {
								if op["prior_input"] != nil {
									return fmt.Errorf("update after move should have null prior_input: %v", op)
								}
								if fmt.Sprint(op["id"]) != "mv" {
									return fmt.Errorf("update after move should carry the id: %v", op)
								}
								if op["prior_output"] != nil {
									return fmt.Errorf("the default move carries only the id, prior_output should be null: %v", op["prior_output"])
								}
							}
						}
						return nil
					}),
				),
			},
			{
				Config:   moved,
				PlanOnly: true,
			},
		},
	})
}
