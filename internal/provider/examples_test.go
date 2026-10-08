// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func examplePath(t *testing.T, name string) string {
	t.Helper()
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "..", "examples", "resources", "scripted_resource", name)
}

// The flagship example, manage.py, on the vendored helper: the whole
// lifecycle, the data source, import, and a replacement from its plan hook.
func TestAccExample_manage(t *testing.T) {
	store := t.TempDir()
	cfg := func(kind string, size int) string {
		return fmt.Sprintf(`
provider "scripted" {
  program = ["python3", %q]
  input   = {}
  environment = { STORE_DIR = %q }
}

resource "scripted_resource" "record" {
  input = { name = "web-1", kind = %q, size = %d }
}

data "scripted_data" "lookup" {
  input      = { name = "web-1" }
  depends_on = [scripted_resource.record]
}
`, examplePath(t, "manage.py"), store, kind, size)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			if _, err := os.Stat(filepath.Join(store, "web-1.json")); err == nil {
				return fmt.Errorf("record still exists after destroy")
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: cfg("vm", 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("scripted_resource.record", "id", "web-1"),
					resource.TestCheckResourceAttr("scripted_resource.record", "output.address", "web-1.internal.example"),
					resource.TestCheckResourceAttr("scripted_resource.record", "output.revision", "1"),
					resource.TestCheckResourceAttr("data.scripted_data.lookup", "output.exists", "true"),
				),
			},
			{
				Config:           cfg("vm", 3),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("scripted_resource.record", plancheck.ResourceActionUpdate)}},
				Check:            resource.TestCheckResourceAttr("scripted_resource.record", "output.revision", "2"),
			},
			{
				// kind is immutable in manage.py's plan hook
				Config:           cfg("container", 3),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("scripted_resource.record", plancheck.ResourceActionReplace)}},
				Check:            resource.TestCheckResourceAttr("scripted_resource.record", "output.revision", "1"),
			},
			{
				Config:            cfg("container", 3),
				ResourceName:      "scripted_resource.record",
				ImportState:       true,
				ImportStateId:     "web-1",
				ImportStateVerify: true,
			},
			{
				Config:      cfg("container", -1),
				ExpectError: regexp.MustCompile(`size must be >= 0`),
			},
		},
	})
}

// startRESTServer runs testdata/rest_server.py on a free port and returns its
// base URL.
func startRESTServer(t *testing.T, token string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address %T", l.Addr())
	}
	port := addr.Port
	_ = l.Close()
	wd, _ := os.Getwd()
	cmd := exec.Command("python3", filepath.Join(wd, "testdata", "rest_server.py"), fmt.Sprint(port), token)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "ready" {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("rest_server.py did not become ready")
	}
	return fmt.Sprintf("http://127.0.0.1:%d/widgets", port)
}

// The REST example against a small HTTP API: create, drift, update, delete.
func TestAccExample_restAPI(t *testing.T) {
	testAccPreCheck(t)
	base := startRESTServer(t, "s3cret")
	cfg := func(size int) string {
		return fmt.Sprintf(`
provider "scripted" {
  program = ["python3", %q]
  input   = { base_url = %q, token = "s3cret" }
}

resource "scripted_resource" "w" {
  input = { name = "w1", size = %d }
}
`, examplePath(t, "rest_api.py"), base, size)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg(2),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("scripted_resource.w", "id", "w1"),
					resource.TestCheckResourceAttr("scripted_resource.w", "output.created_at", "2026-10-08T00:00:00Z"),
				),
			},
			{
				Config:           cfg(5),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("scripted_resource.w", plancheck.ResourceActionUpdate)}},
				Check:            resource.TestCheckResourceAttr("scripted_resource.w", "input.size", "5"),
			},
		},
	})
}

// The helper handles requests concurrently by default: eight creates that
// each take half a second finish together, not one after another.
func TestAccHelper_concurrentByDefault(t *testing.T) {
	wd, _ := os.Getwd()
	dir := t.TempDir()
	config := fmt.Sprintf(`
provider "scripted" {
  program     = ["python3", %q]
  environment = { HB_DIR = %q, HB_SLEEP = "0.5" }
}

resource "scripted_resource" "test" {
  count     = 8
  plan_hook = false
  input     = { name = "c-${count.index}" }
}
`, filepath.Join(wd, "testdata", "helper_backend.py"), dir)
	var started time.Time
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				PreConfig: func() { started = time.Now() },
				Config:    config,
				Check: func(_ *terraform.State) error {
					// Sequential handling would need 8 x 0.5 s = 4 s for the
					// creates alone; concurrent handling takes about one.
					if took := time.Since(started); took > 3500*time.Millisecond {
						return fmt.Errorf("eight 0.5 s creates took %s; the helper is not handling requests concurrently", took)
					}
					return nil
				},
			},
		},
	})
}

// sensitive_environment reaches the program and takes precedence over
// environment, which takes precedence over the provider's.
func TestAccResource_sensitiveEnvironment(t *testing.T) {
	b := newBackend(t)
	config := b.providerBlock("BACKEND_TAG", "provider") + `
resource "scripted_resource" "test" {
  environment           = { BACKEND_TAG = "resource" }
  sensitive_environment = { BACKEND_TAG = "sensitive" }
  input                 = { name = "senv" }
}

resource "scripted_resource" "plain" {
  environment = { BACKEND_TAG = "resource" }
  input       = { name = "senv2" }
}
`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("senv", "senv2"),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(testRes, "output.tag", "sensitive"),
					resource.TestCheckResourceAttr(testRes, "sensitive_environment.BACKEND_TAG", "sensitive"),
					resource.TestCheckResourceAttr("scripted_resource.plain", "output.tag", "resource"),
				),
			},
		},
	})
}

// Private state the plan hook returns is not carried into create (a
// framework limitation the protocol documents) but is carried into update.
func TestAccResource_planPrivateNotOnCreate(t *testing.T) {
	b := newBackend(t)
	cfg := func(size int) string {
		return b.providerBlock() + fmt.Sprintf(`
resource "scripted_resource" "test" {
  input = { name = "pp", size = %d, plan_private = true }
}
`, size)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             b.checkDestroy("pp"),
		Steps: []resource.TestStep{
			{
				Config: cfg(1),
				Check: check(func() error {
					if op := b.lastOp(t, "create"); op["private"] != nil {
						return fmt.Errorf("create should not see the plan hook's private state (documented): %v", op["private"])
					}
					return nil
				}),
			},
			{
				Config: cfg(2),
				Check: check(func() error {
					p, _ := b.lastOp(t, "update")["private"].(map[string]any)
					if p["from_plan"] != "update" {
						return fmt.Errorf("update should see the plan hook's private state: %v", p)
					}
					return nil
				}),
			},
		},
	})
}
