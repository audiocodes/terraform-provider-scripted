// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// testAccProtoV6ProviderFactories is used by every acceptance test to serve
// the provider in-process.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"scripted": providerserver.NewProtocol6WithError(New("test")()),
}

func testAccPreCheck(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for the acceptance tests")
	}
}

// backend is one acceptance test's private copy of the fake backend: a temp
// directory plus the program lines that drive it.
type backend struct {
	dir    string
	script string
}

func newBackend(t *testing.T) backend {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return backend{dir: t.TempDir(), script: filepath.Join(wd, "testdata", "backend.py")}
}

// program renders the program attribute, as HCL.
func (b backend) program() string {
	return fmt.Sprintf(`program = ["python3", %q]`, b.script)
}

// env renders an environment attribute pointing at this backend, with any
// extra variables added.
func (b backend) env(extra ...string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "environment = {\n    BACKEND_DIR = %q\n", b.dir)
	for i := 0; i+1 < len(extra); i += 2 {
		fmt.Fprintf(&sb, "    %s = %q\n", extra[i], extra[i+1])
	}
	sb.WriteString("  }")
	return sb.String()
}

// ops returns every request the backend has received, oldest first.
func (b backend) ops(t *testing.T) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(b.dir, "ops.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("ops.jsonl: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// opNames lists the op of each request, e.g. [create read read update].
func (b backend) opNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, o := range b.ops(t) {
		names = append(names, fmt.Sprint(o["op"]))
	}
	return names
}

func (b backend) count(t *testing.T, op string) int {
	t.Helper()
	n := 0
	for _, name := range b.opNames(t) {
		if name == op {
			n++
		}
	}
	return n
}

// object reads a managed object straight from the backend's storage.
func (b backend) object(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.dir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (b backend) writeObject(t *testing.T, name string, obj map[string]any) {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.dir, name+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (b backend) exists(name string) bool {
	_, err := os.Stat(filepath.Join(b.dir, name+".json"))
	return err == nil
}

// checkDestroy fails if any object is left behind after the test.
func (b backend) checkDestroy(names ...string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		for _, n := range names {
			if b.exists(n) {
				return fmt.Errorf("object %q still exists after destroy", n)
			}
		}
		return nil
	}
}
