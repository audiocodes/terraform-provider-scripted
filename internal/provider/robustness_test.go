// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// A program that asks for replacement on a context-only change is honoured
// when always_update is on, and warned about (not silently ignored) when off.
func TestAccResource_requiresReplaceOnContextChange(t *testing.T) {
	b := newBackend(t)
	cfg := func(n int, alwaysUpdate bool) string {
		return b.providerBlock() + fmt.Sprintf(`
resource "scripted_resource" "test" {
  always_update = %t
  context       = { replace_on_change = true, n = %d }
  input         = { name = "rr" }
}
`, alwaysUpdate, n)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("rr"),
		Steps: []resource.TestStep{
			{Config: cfg(1, true)},
			{
				Config:           cfg(2, true),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionReplace),
				Check:            expectOps(t, b, map[string]int{"create": 2, "delete": 1}),
			},
			{Config: cfg(2, false)},
			{
				// always_update off: an in-place update of context only, the
				// program's wish for replacement surfaces as a warning.
				Config:           cfg(3, false),
				ConfigPlanChecks: expectAction(plancheck.ResourceActionUpdate),
				Check:            expectOps(t, b, map[string]int{"create": 2, "delete": 1}),
			},
		},
	})
}

func TestAccResource_timeout(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: b.providerBlock("BACKEND_SLEEP", "5") + `
resource "scripted_resource" "test" {
  timeout = "1s"
  input   = { name = "slow" }
}
`,
				ExpectError: regexp.MustCompile(`did not answer this request within 1s`),
			},
			{
				Config: `
provider "scripted" {
  timeout = "nonsense"
}
resource "scripted_resource" "test" {
  program = ["true"]
  input   = { name = "x" }
}
`,
				ExpectError: regexp.MustCompile(`Invalid timeout`),
			},
		},
	})
}

// timeout = "0" removes the limit, also over a provider-level one: a request
// that outlasts the provider's timeout succeeds on a resource that sets "0".
func TestAccResource_noTimeout(t *testing.T) {
	b := newBackend(t)
	config := fmt.Sprintf(`
provider "scripted" {
  %s
  %s
  timeout = "1s"
}

resource "scripted_resource" "test" {
  timeout   = "0"
  plan_hook = false
  input     = { name = "unbounded" }
}
`, b.program(), b.env("BACKEND_SLEEP", "2"))
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("unbounded"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  resource.TestCheckResourceAttr(testRes, "id", "unbounded"),
			},
		},
	})
}

// stdout is for responses only: a stray line is not a response and ends the
// program; an empty response to create is an error.
func TestAccResource_badStdout(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: b.providerBlock("BACKEND_GARBAGE", "plan") + `
resource "scripted_resource" "test" {
  input = { name = "garbage" }
}
`,
				ExpectError: regexp.MustCompile(`not a response with a request_id`),
			},
			{
				Config: b.providerBlock("BACKEND_SILENT", "create") + `
resource "scripted_resource" "test" {
  input = { name = "silent" }
}
`,
				ExpectError: regexp.MustCompile(`Create returned no id`),
			},
		},
	})
}

// Terraform runs up to -parallelism operations at once; the provider must
// cope, and in persistent mode a handful of processes must serve them all.
func TestAccResource_concurrency(t *testing.T) {
	b := newBackend(t)
	config := b.providerBlock() + `
resource "scripted_resource" "test" {
  count = 6
  input = { name = "par-${count.index}" }
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("par-0", "par-1", "par-2", "par-3", "par-4", "par-5"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("scripted_resource.test.5", "output.address", "par-5.example"),
					expectOps(t, b, map[string]int{"create": 6}),
				),
			},
			{
				Config: config,
				Check: check(func() error {
					// One program process per Terraform command, however many
					// resources and operations.
					pids := map[string]bool{}
					for _, op := range b.ops(t) {
						pids[fmt.Sprint(op["pid"])] = true
					}
					if n := len(b.ops(t)); len(pids)*4 > n {
						return fmt.Errorf("expected one process to serve many operations: %d ops from %d pids", n, len(pids))
					}
					return nil
				}),
			},
		},
	})
}

// The number of program runs per Terraform command is part of the cost model
// users rely on; pin it so a stray extra hook call shows up.
func TestAccResource_invocationCounts(t *testing.T) {
	b := newBackend(t)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("count"),
		Steps: []resource.TestStep{
			{
				// apply that creates: plan (plan) + plan (apply) + create
				Config: cfg(b, "count", 1),
				Check:  expectOps(t, b, map[string]int{"plan": 2, "create": 1, "read": 0, "update": 0}),
			},
			{
				// apply that changes input: read + plan×2 + update
				// (plus the refresh plans plugin-testing ran after step 1: read + plan)
				Config: cfg(b, "count", 2),
				Check:  expectOps(t, b, map[string]int{"create": 1, "update": 1}),
			},
		},
	})
}

// A program that handles requests in threads serves them all from one
// process at the same time; every request carries a request_id.
func TestAccResource_concurrentProgram(t *testing.T) {
	b := newBackend(t)
	config := b.providerBlock("BACKEND_CONCURRENT", "1", "BACKEND_SLEEP", "0.3") + `
resource "scripted_resource" "test" {
  count = 6
  input = { name = "cc-${count.index}" }
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("cc-0", "cc-1", "cc-2", "cc-3", "cc-4", "cc-5"),
		Steps: []resource.TestStep{
			{Config: config},
			{
				Config: config,
				Check: check(func() error {
					ops := b.ops(t)
					pids := map[string]int{}
					for _, op := range ops {
						pids[fmt.Sprint(op["pid"])]++
					}
					if len(pids)*4 > len(ops) {
						return fmt.Errorf("expected one process per Terraform command, saw %d pids over %d ops: %v", len(pids), len(ops), pids)
					}
					for _, op := range ops {
						if op["request_id"] == nil {
							return fmt.Errorf("every request should carry a request_id: %v", op)
						}
					}
					return nil
				}),
			},
		},
	})
}

// One resource whose create hangs fails on its own; its siblings, served by
// the same program process, are created normally. (Whether the slow create
// still completes behind Terraform's back depends on when the program is
// torn down; that unmanaged object is why the docs tell create authors to
// make a half-finished create recoverable.)
func TestAccResource_slowSiblingIsolated(t *testing.T) {
	b := newBackend(t)
	cfg := func(knobs string, slowName string) string {
		return b.providerBlock() + knobs + fmt.Sprintf(`
resource "scripted_resource" "test" {
  count     = 3
  timeout   = "1s"
  plan_hook = false
  input     = { name = count.index == 1 ? %q : "sib-${count.index}" }
}
`, slowName)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("sib-0", "sib-1b", "sib-2"),
		Steps: []resource.TestStep{
			{
				Config: strings.Replace(cfg("", "sib-1"), b.env(), b.env("BACKEND_CONCURRENT", "1", "BACKEND_SLEEP", "3", "BACKEND_SLEEP_NAME", "sib-1"), 1),
				// The two healthy siblings are created; only the slow one fails.
				ExpectError: regexp.MustCompile(`scripted_resource\.test\[1\][\s\S]*did not answer this request within 1s`),
			},
			{
				Config: cfg("", "sib-1b"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("scripted_resource.test.0", "id", "sib-0"),
					resource.TestCheckResourceAttr("scripted_resource.test.1", "id", "sib-1b"),
					resource.TestCheckResourceAttr("scripted_resource.test.2", "id", "sib-2"),
					check(func() error {
						// sib-0 and sib-2 once, sib-1 (timed out), sib-1b now.
						if n := b.count(t, "create"); n != 4 {
							return fmt.Errorf("expected 4 create requests over both steps, got %d: %v", n, b.opNames(t))
						}
						return nil
					}),
				),
			},
		},
	})
}
