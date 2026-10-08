// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

type tftypesValue = tftypes.Value

// programSettings are the attributes, shared by the resource and the data
// source, that say how to run the program. Embedded in their models so that
// a new attribute reaches both.
type programSettings struct {
	Program              types.List    `tfsdk:"program"`
	WorkingDir           types.String  `tfsdk:"working_dir"`
	Environment          types.Map     `tfsdk:"environment"`
	SensitiveEnvironment types.Map     `tfsdk:"sensitive_environment"`
	Timeout              types.String  `tfsdk:"timeout"`
	Context              types.Dynamic `tfsdk:"context"`
}

func nullProgramSettings() programSettings {
	return programSettings{
		Program:              types.ListNull(types.StringType),
		WorkingDir:           types.StringNull(),
		Environment:          types.MapNull(types.StringType),
		SensitiveEnvironment: types.MapNull(types.StringType),
		Timeout:              types.StringNull(),
		Context:              types.DynamicNull(),
	}
}

// errUnknown says the program cannot be resolved until apply, and which
// attribute is the reason.
type errUnknown struct{ Attr string }

func (e errUnknown) Error() string { return e.Attr + " is not known until apply" }

// errMissingProgram says neither the resource nor the provider names one.
var errMissingProgram = errors.New("no program to run: set `program` on the resource or on the provider block")

// errBadSettings says the configuration could not be read; the details are
// already in the diagnostics.
var errBadSettings = errors.New("invalid program settings")

// firstUnknown names the first attribute that is not known yet, or "".
func (s programSettings) firstUnknown(p *providerData) string {
	switch {
	case s.Program.IsUnknown():
		return "program"
	case s.Program.IsNull() && p == nil:
		return "provider configuration"
	case s.Program.IsNull() && p.programUnknown:
		return "the provider's program"
	case s.WorkingDir.IsUnknown():
		return "working_dir"
	case s.Timeout.IsUnknown():
		return "timeout"
	case s.Environment.IsUnknown():
		return "environment"
	case s.SensitiveEnvironment.IsUnknown():
		return "sensitive_environment"
	case s.Context.IsUnknown():
		return "context"
	case p != nil && p.inputUnknown:
		return "the provider's input"
	}
	if _, unknown := valueToJSON(mustTF(s.Context)); len(unknown) > 0 {
		return "context." + unknown[0]
	}
	return ""
}

// resolve builds the program to run from these settings and the provider's:
// the resource's program else the provider's; environment is provider, then
// resource, then sensitive; timeout likewise. It returns errUnknown,
// errMissingProgram or errBadSettings (with diagnostics added) when it cannot.
func (s programSettings) resolve(ctx context.Context, p *providerData, diags *diag.Diagnostics) (program, error) {
	if attr := s.firstUnknown(p); attr != "" {
		return program{}, errUnknown{Attr: attr}
	}
	prog := program{Env: map[string]string{}, InheritEnvironment: true}

	if !s.Program.IsNull() {
		diags.Append(s.Program.ElementsAs(ctx, &prog.Args, false)...)
	} else if p != nil {
		prog.Args = p.program
	}
	if diags.HasError() {
		return program{}, errBadSettings
	}
	if len(prog.Args) == 0 {
		return program{}, errMissingProgram
	}

	if p != nil {
		prog.WorkingDir = p.workingDir
		for k, v := range p.environment {
			prog.Env[k] = v
		}
		prog.InheritEnvironment = p.inheritEnvironment
		prog.Timeout = p.timeout
		prog.ProviderInput = p.input
	}
	if s.WorkingDir.ValueString() != "" {
		prog.WorkingDir = s.WorkingDir.ValueString()
	}
	for _, src := range []types.Map{s.Environment, s.SensitiveEnvironment} {
		if src.IsNull() {
			continue
		}
		var extra map[string]string
		diags.Append(src.ElementsAs(ctx, &extra, false)...)
		for k, v := range extra {
			prog.Env[k] = v
		}
	}
	if t := parseTimeout(s.Timeout, diags); t != 0 {
		prog.Timeout = t
	}
	if diags.HasError() {
		return program{}, errBadSettings
	}
	prog.Context, _ = valueToJSON(mustTF(s.Context))
	return prog, nil
}

// mustTF unwraps a dynamic value whose conversion cannot fail (it was built
// by the framework from configuration or state).
func mustTF(d types.Dynamic) tftypesValue {
	v, err := d.ToTerraformValue(context.Background())
	if err != nil {
		panic(fmt.Sprintf("dynamic value from the framework failed to convert: %v", err))
	}
	return v
}

// parseTimeout turns a duration attribute into a time.Duration: zero for
// null (use the default), noTimeout for "0" (no bound).
func parseTimeout(s types.String, diags *diag.Diagnostics) time.Duration {
	if s.IsNull() || s.IsUnknown() || s.ValueString() == "" {
		return 0
	}
	d, err := time.ParseDuration(s.ValueString())
	if err != nil || d < 0 {
		diags.AddError("Invalid timeout", fmt.Sprintf("%q is not a duration such as \"30s\", \"10m\" or \"1h\", "+
			"nor \"0\" for no limit.", s.ValueString()))
		return 0
	}
	if d == 0 {
		return noTimeout
	}
	return d
}

// mustResolve resolves the program for an operation that cannot proceed
// without it (create, update, delete, import, data), adding a diagnostic
// when it cannot. ok is false when the caller should stop.
func mustResolve(ctx context.Context, s programSettings, p *providerData, diags *diag.Diagnostics) (program, bool) {
	prog, err := s.resolve(ctx, p, diags)
	switch {
	case err == nil:
		return prog, true
	case errors.Is(err, errBadSettings):
		return program{}, false
	case errors.Is(err, errMissingProgram):
		diags.AddError("No program to run", "Set `program` on the resource or on the provider block.")
		return program{}, false
	default:
		diags.AddError("Program not known", "The program must be fully known at apply time: "+err.Error()+".")
		return program{}, false
	}
}
