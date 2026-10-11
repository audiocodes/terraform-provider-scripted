// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

func TestResponse_inapplicable(t *testing.T) {
	t.Parallel()
	id := "x"
	yes := true
	r := &response{ID: &id, Exists: &yes, RequiresReplace: true, Output: json.RawMessage(`{}`)}
	cases := map[string][]string{
		"plan":   {"exists", "id"},
		"create": {"exists", "requires_replace"},
		"read":   {"requires_replace"},
		"update": {"exists", "requires_replace"},
		"delete": {"exists", "id", "output", "requires_replace"},
		"import": {"exists", "requires_replace"},
		"data":   {"exists", "id", "requires_replace"},
	}
	for op, want := range cases {
		if got := r.inapplicable(op); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", op, got, want)
		}
	}
	if got := r.inapplicable("future_op"); got != nil {
		t.Errorf("unknown op should not warn, got %v", got)
	}
	var diags diag.Diagnostics
	if r.report(&diags, "create") {
		t.Error("report should not fail without errors")
	}
	if diags.WarningsCount() != 1 || !strings.Contains(diags.Warnings()[0].Detail(), "exists, requires_replace") {
		t.Errorf("expected one warning naming the fields, got %v", diags)
	}
}

func TestResponse_errorsAndNotImplemented(t *testing.T) {
	t.Parallel()
	var diags diag.Diagnostics
	r := &response{Errors: []string{"a", "b"}, Warnings: []string{"w"}}
	if !r.report(&diags, "update") {
		t.Error("errors should fail the operation")
	}
	if diags.ErrorsCount() != 2 || diags.WarningsCount() != 1 {
		t.Errorf("diagnostics: %v", diags)
	}
	var nilResp *response
	if nilResp.notImplemented() || nilResp.hasInput() || nilResp.errorList() != nil {
		t.Error("nil response helpers must be safe and false")
	}
	if !(&response{NotImplemented: true}).notImplemented() {
		t.Error("not_implemented not detected")
	}
}

func TestParseResponse(t *testing.T) {
	t.Parallel()
	var r response
	if err := parseResponse([]byte(` {"id": "a"} `+"\n"), &r); err != nil || *r.ID != "a" {
		t.Errorf("plain JSON: %v %v", err, r.ID)
	}
	if err := parseResponse([]byte(`{"id": "a"} {"id": "b"}`), &r); err == nil || !strings.Contains(err.Error(), "more than one JSON document") {
		t.Errorf("two documents on a line should fail: %v", err)
	}
	if err := parseResponse([]byte("debug line"), &r); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("non-JSON should fail: %v", err)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("é", maxMessage) // 2 bytes each, so the cut lands mid-rune
	got := truncate(s)
	if !strings.HasSuffix(got, "(truncated)") {
		t.Fatal("not truncated")
	}
	body := strings.TrimSuffix(got, "\n... (truncated)")
	if strings.ToValidUTF8(body, "?") != body {
		t.Error("truncate split a rune")
	}
	if len(body) > maxMessage {
		t.Errorf("body longer than limit: %d", len(body))
	}
}

func TestProgramEnvironment(t *testing.T) {
	t.Setenv("SCRIPTED_TEST_SECRET", "s")
	p := program{Env: map[string]string{"B": "2", "A": "1"}}
	p.InheritEnvironment = true
	envList, _ := p.environment()
	env := strings.Join(envList, "\n")
	if !strings.Contains(env, "SCRIPTED_TEST_SECRET=s") || !strings.Contains(env, "SCRIPTED_PROTOCOL=1") || !strings.Contains(env, "\nA=1\nB=2") {
		t.Errorf("inherited environment wrong:\n%s", env)
	}
	p.InheritEnvironment = false
	envList, _ = p.environment()
	env = strings.Join(envList, "\n")
	if strings.Contains(env, "SCRIPTED_TEST_SECRET") || !strings.Contains(env, "PATH=") || !strings.Contains(env, "A=1") {
		t.Errorf("clean environment wrong:\n%s", env)
	}
}

// The helper's directory goes on PYTHONPATH after whatever the user set
// there, the helper's path is passed on, and both are reported as injected.
func TestProgramEnvironment_pythonHelper(t *testing.T) {
	sep := string(os.PathListSeparator)
	dir := filepath.Join(t.TempDir(), "python")
	helper := filepath.Join(dir, "scripted.py")

	t.Setenv("PYTHONPATH", "")
	p := program{PythonHelperDir: dir, InheritEnvironment: true}
	env, injected := p.environment()
	if got := lookupEnv(env, "PYTHONPATH"); got != dir {
		t.Errorf("PYTHONPATH = %q, want the helper's directory %q", got, dir)
	}
	if got := lookupEnv(env, "SCRIPTED_PYTHON_HELPER"); got != helper {
		t.Errorf("SCRIPTED_PYTHON_HELPER = %q, want %q", got, helper)
	}
	want := []envVar{{"SCRIPTED_PROTOCOL", "1"}, {"PYTHONPATH", dir}, {"SCRIPTED_PYTHON_HELPER", helper}}
	if fmt.Sprint(injected) != fmt.Sprint(want) {
		t.Errorf("injected = %v, want %v", injected, want)
	}

	t.Setenv("PYTHONPATH", "inherited")
	env, _ = p.environment()
	if got := lookupEnv(env, "PYTHONPATH"); got != "inherited"+sep+dir {
		t.Errorf("inherited PYTHONPATH should come first, got %q", got)
	}
	p.Env = map[string]string{"PYTHONPATH": "configured"}
	env, _ = p.environment()
	if got := lookupEnv(env, "PYTHONPATH"); got != "configured"+sep+dir {
		t.Errorf("configured PYTHONPATH should come first, got %q", got)
	}

	env, injected = (program{InheritEnvironment: true}).environment()
	if lookupEnv(env, "SCRIPTED_PYTHON_HELPER") != "" || len(injected) != 1 {
		t.Errorf("without a helper only SCRIPTED_PROTOCOL is injected: %v", injected)
	}
}

func TestLookupEnv(t *testing.T) {
	t.Parallel()
	env := []string{"A=1", "B=x=y", "A=2", "broken"}
	if lookupEnv(env, "A") != "2" || lookupEnv(env, "B") != "x=y" || lookupEnv(env, "C") != "" {
		t.Error("lookupEnv should return the last value of a name, with any = in it")
	}
	want := ""
	if runtime.GOOS == "windows" {
		want = "2"
	}
	if got := lookupEnv(env, "a"); got != want {
		t.Errorf("lookupEnv(a) = %q on %s", got, runtime.GOOS)
	}
}

func TestReadLine(t *testing.T) {
	t.Parallel()
	r := bufio.NewReaderSize(strings.NewReader("short\n"+strings.Repeat("x", 100)+"\n"), 16)
	if line, err := readLine(r, 1000); err != nil || string(line) != "short\n" {
		t.Errorf("short line: %q %v", line, err)
	}
	if line, err := readLine(r, 1000); err != nil || len(line) != 101 {
		t.Errorf("line longer than the buffer: %d %v", len(line), err)
	}
	r = bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 100)+"\n"), 16)
	if _, err := readLine(r, 50); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Errorf("limit should apply: %v", err)
	}
}

func TestRollingBuffer(t *testing.T) {
	t.Parallel()
	b := &rollingBuffer{limit: 5}
	_, _ = b.Write([]byte("abcdefgh"))
	if got := b.String(); got != "...defgh" {
		t.Errorf("got %q", got)
	}
	if got := b.TakeString(); got != "...defgh" || b.String() != "" {
		t.Errorf("take: %q then %q", got, b.String())
	}
}
