// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// phase is what Terraform intends for the resource in a plan.
type phase int

const (
	phaseNone   phase = iota // nothing changed
	phaseCreate              // no prior state
	phaseUpdate              // some attribute changed
	phaseDelete              // no planned state
)

func (ph phase) String() string {
	return [...]string{"none", "create", "update", "delete"}[ph]
}

// change is a plan, classified: the models involved and what differs.
type change struct {
	phase        phase
	plan, state  resourceModel
	planInput    tftypes.Value
	priorInput   tftypes.Value
	priorOutput  tftypes.Value
	inputChanged bool
	changed      []string // top-level attributes that differ, input first
}

// effective is the model whose program and hooks apply: the planned one,
// or for a destroy the one in state.
func (c change) effective() *resourceModel {
	if c.phase == phaseDelete {
		return &c.state
	}
	return &c.plan
}

// willRun reports whether the program's update or create will be called
// for this change.
func (c change) willRun() bool {
	return c.phase == phaseCreate || (c.phase == phaseUpdate && (c.inputChanged || c.plan.AlwaysUpdate.ValueBool()))
}

// classifyChange reads the plan and state and works out the phase.
func classifyChange(ctx context.Context, req resource.ModifyPlanRequest) (change, diag.Diagnostics) {
	var c change
	var diags diag.Diagnostics
	creating := req.State.Raw.IsNull()
	destroying := req.Plan.Raw.IsNull()
	if !destroying {
		diags.Append(req.Plan.Get(ctx, &c.plan)...)
	}
	if !creating {
		diags.Append(req.State.Get(ctx, &c.state)...)
	}
	if diags.HasError() {
		return c, diags
	}
	switch {
	case destroying:
		c.phase = phaseDelete
		c.priorInput = dynamicToTF(ctx, c.state.Input, &diags)
		c.priorOutput = dynamicToTF(ctx, c.state.Output, &diags)
	case creating:
		c.phase = phaseCreate
		c.inputChanged = true
		c.planInput = dynamicToTF(ctx, c.plan.Input, &diags)
	default:
		c.planInput = dynamicToTF(ctx, c.plan.Input, &diags)
		c.priorInput = dynamicToTF(ctx, c.state.Input, &diags)
		c.priorOutput = dynamicToTF(ctx, c.state.Output, &diags)
		c.inputChanged = !jsonEqual(c.priorInput, c.planInput)
		c.changed = changedAttributes(req.Plan.Raw, req.State.Raw)
		if len(c.changed) > 0 {
			c.phase = phaseUpdate
		}
	}
	return c, diags
}

// planRequest is what the plan hook is told.
func (c change) planRequest(private any) request {
	rq := request{Op: "plan", Action: c.phase.String(), Private: private}
	if c.phase != phaseDelete {
		rq.Input, rq.Unknown = valueToJSON(c.planInput)
		rq.InputChanged = boolPtr(c.inputChanged)
	}
	if c.phase != phaseCreate {
		rq.ID = idPtr(c.state.ID)
		rq.PriorInput, _ = valueToJSON(c.priorInput)
		rq.PriorOutput, _ = valueToJSON(c.priorOutput)
		if c.phase == phaseDelete {
			rq.Input = rq.PriorInput
		}
	}
	return rq
}

// replaceDecision says what a program's request for replacement means for
// this change: the attribute to attach the replacement to (Terraform needs
// one that changed), or a warning when it cannot apply.
func replaceDecision(c change) (attr string, warning string) {
	switch {
	case c.phase == phaseUpdate && c.willRun():
		return c.changed[0], ""
	case c.phase == phaseUpdate:
		return "", "The program asked for replacement, but only program settings changed and `always_update` is " +
			"off, so the update will not run. Set `always_update = true` to let the program act on such changes."
	case c.phase == phaseCreate:
		return "", "" // creating already; nothing to replace
	default:
		return "", fmt.Sprintf("The program asked for replacement during a plan with action %q, where it has no effect.", c.phase)
	}
}

// applyPlanResponse carries the plan hook's answer into the plan.
func applyPlanResponse(ctx context.Context, c change, out *response, resp *resource.ModifyPlanResponse) {
	if out.RequiresReplace {
		if attr, warning := replaceDecision(c); attr != "" {
			resp.RequiresReplace = append(resp.RequiresReplace, path.Root(attr))
		} else if warning != "" {
			resp.Diagnostics.AddWarning("Replacement not applied", warning)
		}
	}
	if out.Output != nil && c.willRun() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("output"),
			jsonToDynamic(ctx, out.Output, nil, &resp.Diagnostics))...)
	}
}

// ModifyPlan keeps output stable when nothing will run and gives the program
// a say over the plan through the plan hook. Configuration checks happen in
// the hook too: the provider, and so its program and environment, is not yet
// configured when Terraform validates configuration.
func (r *scriptedResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	c, diags := classifyChange(ctx, req)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if c.phase == phaseUpdate && !c.willRun() {
		// The framework marked output unknown because something in the
		// config changed; nothing will run, so the old output stands.
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("output"), c.state.Output)...)
	}

	prog, err := c.effective().resolve(ctx, r.provider, &resp.Diagnostics)
	switch {
	case errors.Is(err, errBadSettings):
		return
	case errors.Is(err, errMissingProgram):
		if c.phase != phaseDelete {
			resp.Diagnostics.AddError("No program to run", "Set `program` on the resource or on the provider block.")
		}
		return
	case err != nil:
		tflog.Debug(ctx, "plan hook skipped: "+err.Error())
		return
	}
	if !hookEnabled(c.effective().PlanHook) {
		return
	}

	rq := c.planRequest(readPrivate(ctx, req.Private, &resp.Diagnostics))
	out, result := prog.do(ctx, rq, &resp.Diagnostics, resp.Private, false)
	if result != doOK {
		return
	}
	applyPlanResponse(ctx, c, out, resp)
}
