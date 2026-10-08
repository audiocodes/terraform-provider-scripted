// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The requires_replace matrix, as a table.
func TestReplaceDecision(t *testing.T) {
	t.Parallel()
	always := emptyModel()
	always.AlwaysUpdate = types.BoolValue(true)
	cases := []struct {
		name     string
		c        change
		wantAttr string
		wantWarn bool
	}{
		{"update with input change", change{phase: phaseUpdate, inputChanged: true, changed: []string{"input"}, plan: emptyModel()}, "input", false},
		{"update, context only, always_update", change{phase: phaseUpdate, changed: []string{"context"}, plan: always}, "context", false},
		{"update, context only, no always_update", change{phase: phaseUpdate, changed: []string{"context"}, plan: emptyModel()}, "", true},
		{"create", change{phase: phaseCreate, inputChanged: true, plan: emptyModel()}, "", false},
		{"delete", change{phase: phaseDelete, state: emptyModel()}, "", true},
		{"no change", change{phase: phaseNone, plan: emptyModel()}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attr, warning := replaceDecision(tc.c)
			if attr != tc.wantAttr || (warning != "") != tc.wantWarn {
				t.Errorf("got attr=%q warning=%q, want attr=%q warning=%v", attr, warning, tc.wantAttr, tc.wantWarn)
			}
		})
	}
}

func TestChange_willRunAndAction(t *testing.T) {
	t.Parallel()
	always := emptyModel()
	always.AlwaysUpdate = types.BoolValue(true)
	if !(change{phase: phaseCreate}).willRun() {
		t.Error("create runs")
	}
	if (change{phase: phaseUpdate, plan: emptyModel()}).willRun() {
		t.Error("metadata-only update does not run")
	}
	if !(change{phase: phaseUpdate, plan: always}).willRun() {
		t.Error("always_update runs")
	}
	if (change{phase: phaseDelete}).willRun() || (change{phase: phaseNone}).willRun() {
		t.Error("delete and none do not run")
	}
	for ph, want := range map[phase]string{phaseNone: "none", phaseCreate: "create", phaseUpdate: "update", phaseDelete: "delete"} {
		if ph.String() != want {
			t.Errorf("%d: %s", ph, ph.String())
		}
	}
	rq := (change{phase: phaseDelete, state: emptyModel()}).planRequest(nil)
	if rq.Action != "delete" || rq.InputChanged != nil {
		t.Errorf("delete request: %+v", rq)
	}
}

// Which attribute keeps the program from being resolved is named.
func TestProgramSettings_firstUnknown(t *testing.T) {
	t.Parallel()
	configured := &providerData{program: []string{"true"}, environment: map[string]string{}}
	s := nullProgramSettings()
	if got := s.firstUnknown(configured); got != "" {
		t.Errorf("all known: %q", got)
	}
	if got := s.firstUnknown(nil); got != "provider configuration" {
		t.Errorf("unconfigured provider: %q", got)
	}
	if got := s.firstUnknown(&providerData{programUnknown: true}); got != "the provider's program" {
		t.Errorf("provider program unknown: %q", got)
	}
	if got := s.firstUnknown(&providerData{program: []string{"true"}, inputUnknown: true}); got != "the provider's input" {
		t.Errorf("provider input unknown: %q", got)
	}
	s.Program = types.ListUnknown(types.StringType)
	if got := s.firstUnknown(configured); got != "program" {
		t.Errorf("program unknown: %q", got)
	}
	s = nullProgramSettings()
	s.Timeout = types.StringUnknown()
	if got := s.firstUnknown(configured); got != "timeout" {
		t.Errorf("timeout unknown: %q", got)
	}
	s = nullProgramSettings()
	s.Context = types.DynamicUnknown()
	if got := s.firstUnknown(configured); got != "context" {
		t.Errorf("context unknown: %q", got)
	}

	var diags diag.Diagnostics
	_, err := s.resolve(context.Background(), configured, &diags)
	var unknown errUnknown
	if !errors.As(err, &unknown) || unknown.Attr != "context" {
		t.Errorf("resolve should report the unknown attribute: %v", err)
	}
	_, err = nullProgramSettings().resolve(context.Background(), &providerData{}, &diags)
	if !errors.Is(err, errMissingProgram) {
		t.Errorf("no program anywhere: %v", err)
	}
	prog, err := nullProgramSettings().resolve(context.Background(), configured, &diags)
	if err != nil || prog.Args[0] != "true" || prog.Timeout != 0 {
		t.Errorf("provider program: %v %+v", err, prog)
	}
}
