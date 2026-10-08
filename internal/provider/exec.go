// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// protocolVersion is the protocol this provider speaks. It only ever goes up
// when an existing field or operation changes meaning; additions do not bump
// it. A program may declare the version it was written for in any response;
// the provider refuses versions it does not know.
const protocolVersion = 1

// request is the JSON document the program receives for every operation.
// Fields that do not apply to an operation are null or absent.
type request struct {
	Protocol     int         `json:"protocol"`
	RequestID    string      `json:"request_id"`
	Op           string      `json:"op"`
	Action       string      `json:"action,omitempty"`
	ID           *string     `json:"id"`
	Input        any         `json:"input"`
	PriorInput   any         `json:"prior_input"`
	PriorOutput  any         `json:"prior_output"`
	InputChanged *bool       `json:"input_changed,omitempty"`
	Unknown      []string    `json:"unknown,omitempty"`
	Private      any         `json:"private"`
	Source       *moveSource `json:"source,omitempty"`
	// Filled in by program.run from the program's settings.
	ProviderInput any `json:"provider_input"`
	Context       any `json:"context"`
}

// moveSource describes the resource a `moved` block is coming from.
type moveSource struct {
	Provider      string `json:"provider"`
	Type          string `json:"type"`
	SchemaVersion int64  `json:"schema_version"`
	State         any    `json:"state"`
}

// response is what the program prints for a request. Every field but
// request_id is optional; which ones are read depends on the operation (see
// applicable). Unknown fields are ignored; known fields that do not apply to
// the operation draw a warning so that script bugs are visible.
type response struct {
	RequestID       *string           `json:"request_id"`
	Protocol        *int              `json:"protocol"`
	NotImplemented  bool              `json:"not_implemented"`
	ID              *string           `json:"id"`
	Output          json.RawMessage   `json:"output"`
	Input           json.RawMessage   `json:"input"`
	Exists          *bool             `json:"exists"`
	RequiresReplace bool              `json:"requires_replace"`
	Warnings        []string          `json:"warnings"`
	Errors          []string          `json:"errors"`
	Private         json.RawMessage   `json:"private"`
	Program         []string          `json:"program"`
	WorkingDir      *string           `json:"working_dir"`
	Environment     map[string]string `json:"environment"`
	Context         json.RawMessage   `json:"context"`
	// DurationMS is how long the program spent on the request, if it says;
	// the provider logs it next to the time the request waited in line.
	DurationMS *float64 `json:"duration_ms"`
}

// applicable lists, per operation, the response fields the provider acts on.
// request_id, protocol, not_implemented, warnings and errors apply everywhere.
var applicable = map[string][]string{
	"plan":   {"requires_replace", "output", "private"},
	"create": {"id", "output", "private"},
	"read":   {"exists", "input", "output", "id", "private"},
	"update": {"output", "private", "id"},
	"delete": {},
	"import": {"id", "input", "output", "program", "working_dir", "environment", "context", "private"},
	"move":   {"id", "input", "output", "program", "working_dir", "environment", "context", "private"},
	"data":   {"output"},
}

// hasInput reports whether the program said anything about input at all; an
// explicit null is a statement, an absent key is not.
func (r *response) hasInput() bool {
	return r != nil && r.Input != nil
}

// notImplemented reports whether the program declined the operation, asking
// for the provider's default behaviour.
func (r *response) notImplemented() bool {
	return r != nil && r.NotImplemented
}

// errorList gathers the error messages of a response.
func (r *response) errorList() []string {
	if r == nil {
		return nil
	}
	return r.Errors
}

// inapplicable returns the fields the program set that mean nothing for op,
// so that a `requires_replace` from create or an `exists` from update does
// not vanish silently.
func (r *response) inapplicable(op string) []string {
	if r == nil {
		return nil
	}
	set := map[string]bool{
		"id":               r.ID != nil,
		"output":           r.Output != nil,
		"input":            r.Input != nil,
		"exists":           r.Exists != nil,
		"requires_replace": r.RequiresReplace,
		"private":          r.Private != nil,
		"program":          len(r.Program) > 0,
		"working_dir":      r.WorkingDir != nil,
		"environment":      r.Environment != nil,
		"context":          r.Context != nil,
	}
	allowed, known := applicable[op]
	if !known {
		return nil
	}
	for _, f := range allowed {
		delete(set, f)
	}
	var out []string
	for f, isSet := range set {
		if isSet {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// report adds the response's warnings and errors to diags, warns about
// fields that do not apply to op, and reports whether the operation failed.
func (r *response) report(diags *diag.Diagnostics, op string) bool {
	if r == nil {
		return false
	}
	for _, w := range r.Warnings {
		diags.AddWarning(fmt.Sprintf("Program warning (%s)", op), w)
	}
	if extra := r.inapplicable(op); len(extra) > 0 && !r.NotImplemented {
		diags.AddWarning(fmt.Sprintf("Program response fields ignored (%s)", op),
			fmt.Sprintf("The program returned %s, which the provider does not act on for the %s operation. "+
				"See the protocol documentation for which fields each operation reads.", strings.Join(extra, ", "), op))
	}
	errs := r.errorList()
	for _, e := range errs {
		diags.AddError(fmt.Sprintf("Program error (%s)", op), e)
	}
	return len(errs) > 0
}

// program is everything needed to talk to the user's program.
type program struct {
	Args       []string
	WorkingDir string
	Env        map[string]string
	// InheritEnvironment passes Terraform's own environment on to the
	// program; when false only Env and a minimal set of variables are set.
	InheritEnvironment bool
	// Timeout bounds one request; zero means defaultTimeout.
	Timeout time.Duration
	// Grace overrides defaultGrace when stopping the program (tests).
	Grace time.Duration
	// ProviderInput and Context ride along with every request.
	ProviderInput any
	Context       any
}

// maxResponse is the longest response line accepted from a program.
const maxResponse = 64 << 20

// defaultTimeout bounds a request when neither the provider nor the
// resource sets one, so that a program that never answers becomes a
// message rather than a Terraform run that hangs.
const defaultTimeout = 10 * time.Minute

// effectiveTimeout is the request bound to apply.
func (p program) effectiveTimeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return defaultTimeout
}

// defaultGrace is how long a program gets at each step of being stopped:
// after its stdin closes, and again after SIGTERM.
const defaultGrace = 10 * time.Second

// sigterm is what a program receives when it must stop; on platforms where
// signalling is not supported the kill that follows does the job.
var sigterm os.Signal = syscall.SIGTERM

// environment builds the program's environment: Terraform's own (unless
// inheritance is off), then the provider's and resource's variables.
func (p program) environment() []string {
	var env []string
	if p.InheritEnvironment {
		env = os.Environ()
	} else {
		// Enough for an interpreter to start and find its modules.
		for _, k := range []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL", "SYSTEMROOT", "USERPROFILE"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	env = append(env, fmt.Sprintf("SCRIPTED_PROTOCOL=%d", protocolVersion))
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+p.Env[k])
	}
	return env
}

// parseResponse decodes exactly one JSON object from a response line.
func parseResponse(raw []byte, resp *response) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(resp); err != nil {
		return fmt.Errorf("the program printed invalid JSON: %w", err)
	}
	if dec.More() {
		return errors.New("the program printed more than one JSON document on a line")
	}
	return nil
}

const maxMessage = 8 * 1024

// truncate shortens s to about maxMessage bytes without splitting a rune.
func truncate(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	cut := maxMessage
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n... (truncated)"
}
