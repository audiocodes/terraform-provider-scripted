// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// pendingMoveKey holds, in private state, the source of a moved block until
// the program has had a chance to look at it.
const pendingMoveKey = "pending_move"

// MoveState lets a `moved` block bring state over from another resource type.
//
// Terraform asks for the move before it configures the provider, so no
// program can run here. The source state is therefore kept aside in private
// state and the first read that follows (in the same plan, with the provider
// configured) hands it to the program's move operation. Until then the state
// carries only an `id`, when the source has one.
func (r *scriptedResource) MoveState(_ context.Context) []resource.StateMover {
	return []resource.StateMover{{StateMover: r.moveState}}
}

func (r *scriptedResource) moveState(ctx context.Context, req resource.MoveStateRequest, resp *resource.MoveStateResponse) {
	if req.SourceRawState == nil {
		resp.Diagnostics.AddError("Cannot move state", "the source state is empty")
		return
	}
	srcState, err := decodeJSON(req.SourceRawState.JSON)
	if err != nil {
		resp.Diagnostics.AddError("Cannot move state", fmt.Sprintf("decoding source state: %s", err))
		return
	}
	model := emptyModel()
	if m, ok := srcState.(map[string]any); ok {
		if id, ok := m["id"].(string); ok && id != "" {
			model.ID = types.StringValue(id)
		}
	}

	pending, err := json.Marshal(moveSource{
		Provider:      req.SourceProviderAddress,
		Type:          req.SourceTypeName,
		SchemaVersion: req.SourceSchemaVersion,
		State:         srcState,
	})
	if err != nil {
		resp.Diagnostics.AddError("Cannot move state", fmt.Sprintf("encoding source state: %s", err))
		return
	}
	if resp.TargetPrivate != nil {
		resp.Diagnostics.Append(resp.TargetPrivate.SetKey(ctx, pendingMoveKey, pending)...)
	}
	setIdentity(ctx, resp.TargetIdentity, model.ID, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.TargetState.Set(ctx, &model)...)
}

// completePendingMove runs the program's move operation for a moved block
// recorded by moveState, updating the model in place. It reports whether the
// move is settled (handled by the program or declined by it) so the caller
// can clear the pending record; when the program is not available yet the
// record stays for a later read.
func (r *scriptedResource) completePendingMove(ctx context.Context, prog program, m *resourceModel, raw []byte, priv privateWriter, diags *diag.Diagnostics) bool {
	var src moveSource
	if err := json.Unmarshal(raw, &src); err != nil {
		diags.AddWarning("Ignoring unreadable pending move", err.Error())
		return true
	}
	out, result := prog.do(ctx, request{Op: "move", ID: idPtr(m.ID), Source: &src}, diags, priv, false)
	switch result {
	case doFailed:
		return false
	case doDeclined:
		return true // the id-only default stands
	}
	if out.ID == nil || *out.ID == "" {
		if m.ID.IsNull() {
			diags.AddError("Move returned no id",
				"The program must return a non-empty \"id\" from move (the source state had none), or not_implemented.")
			return false
		}
	} else {
		m.ID = types.StringValue(*out.ID)
	}
	if out.hasInput() {
		m.Input = jsonToDynamic(ctx, out.Input, nil, diags)
	}
	if out.Output != nil {
		m.Output = jsonToDynamic(ctx, out.Output, nil, diags)
	}
	applyProgramOverrides(ctx, m, out, diags)
	return true
}
