// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var (
	_ resource.Resource                = (*scriptedResource)(nil)
	_ resource.ResourceWithConfigure   = (*scriptedResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*scriptedResource)(nil)
	_ resource.ResourceWithMoveState   = (*scriptedResource)(nil)
	_ resource.ResourceWithImportState = (*scriptedResource)(nil)
	_ resource.ResourceWithIdentity    = (*scriptedResource)(nil)
)

// privateKey is where the program's opaque private state lives.
const privateKey = "script"

type scriptedResource struct {
	provider *providerData
}

type resourceModel struct {
	programSettings
	ID           types.String  `tfsdk:"id"`
	Input        types.Dynamic `tfsdk:"input"`
	PlanHook     types.Bool    `tfsdk:"plan_hook"`
	AlwaysUpdate types.Bool    `tfsdk:"always_update"`
	Output       types.Dynamic `tfsdk:"output"`
}

// emptyModel is a model with every attribute null except the hook defaults,
// the shape import and moved blocks start from.
func emptyModel() resourceModel {
	return resourceModel{
		programSettings: nullProgramSettings(),
		ID:              types.StringNull(),
		Input:           types.DynamicNull(),
		PlanHook:        types.BoolValue(true),
		AlwaysUpdate:    types.BoolValue(false),
		Output:          types.DynamicNull(),
	}
}

// identityModel is the resource identity: the one value that never changes
// for the life of the object. Program and context cannot be part of it,
// since Terraform forbids identity changes without replacement.
type identityModel struct {
	ID types.String `tfsdk:"id"`
}

func (r *scriptedResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_resource"
}

func (r *scriptedResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = identityschema.Schema{
		Attributes: map[string]identityschema.Attribute{
			"id": identityschema.StringAttribute{
				RequiredForImport: true,
				Description:       "The id the program gave the object; what `import { identity = { id = ... } }` takes.",
			},
		},
	}
}

// setIdentity records the resource identity when the framework asks for one.
func setIdentity(ctx context.Context, identity *tfsdk.ResourceIdentity, id types.String, diags *diag.Diagnostics) {
	if identity == nil || id.IsNull() || id.IsUnknown() {
		return
	}
	diags.Append(identity.Set(ctx, identityModel{ID: id})...)
}

func (r *scriptedResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A resource whose whole lifecycle is delegated to a program you write. " +
			"Terraform does the planning, diffing and state keeping; the program talks to the " +
			"system being managed. See the protocol section below for what the program receives " +
			"and must return.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Identifier returned by the program on create (or import). Passed back to it on every later operation.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"program": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Command and arguments to run for every operation, for example " +
					"`[\"python3\", \"${path.module}/manage.py\"]`. Defaults to the provider's `program`. " +
					"A relative path is resolved against `working_dir`. Changing this never runs the program by itself.",
				Validators: []validator.List{listvalidator.SizeAtLeast(1)},
			},
			"working_dir": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Directory the program runs in. Defaults to the provider's `working_dir`, then Terraform's working directory.",
			},
			"environment": schema.MapAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "Environment variables for the program, on top of the provider's `environment`.",
			},
			"sensitive_environment": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Sensitive:   true,
				MarkdownDescription: "Like `environment`, but hidden in plan output. Stored in state like any " +
					"other attribute; the provider's `environment` and `input` are the ways to keep values out of state.",
			},
			"timeout": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "How long the program may take to answer one request for this resource, as a " +
					"duration such as `\"30m\"`, or `\"0\"` for no limit. Defaults to the provider's `timeout`, else " +
					"10 minutes.",
			},
			"input": schema.DynamicAttribute{
				Optional: true,
				MarkdownDescription: "Desired state of the managed resource, in whatever shape the program " +
					"understands (an object, usually). It is sent to the program on create and update, " +
					"and the program may refresh it on read so that changes made outside Terraform show " +
					"up as drift. The update operation only runs when this value changes (see `always_update`). " +
					"Mark individual values with Terraform's `sensitive()` to hide them in plans.",
			},
			"context": schema.DynamicAttribute{
				Optional: true,
				MarkdownDescription: "Values for the program that are not part of the managed state, such as " +
					"which object the program is in charge of. Sent as `context` with every operation. " +
					"Changing it does not run `update` (unless `always_update`).",
			},
			"plan_hook": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Run the program with `op = \"plan\"` on every plan, letting it validate the " +
					"configuration, ask for replacement, supply output values that are known in advance, or add " +
					"warnings. A program that does not implement `plan` answers `not_implemented`; set this to " +
					"false to skip the call altogether.",
			},
			"always_update": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Run `update` whenever Terraform applies any change to this resource, not " +
					"only when `input` changed. Lets the program decide for itself what a change to " +
					"`program`, `context` or `environment` means, replacement included.",
			},
			"output": schema.DynamicAttribute{
				Computed: true,
				MarkdownDescription: "Whatever the program returned under `output` from the last create, " +
					"update or read: identifiers, addresses, and other facts about the managed resource.",
			},
		},
	}
}

func (r *scriptedResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("Expected *providerData, got %T. This is a bug in the provider.", req.ProviderData))
		return
	}
	r.provider = data
}

// dynamicToTF unwraps a types.Dynamic into the tftypes value underneath,
// which is what the conversion helpers work on.
func dynamicToTF(ctx context.Context, d types.Dynamic, diags *diag.Diagnostics) tftypes.Value {
	v, err := d.ToTerraformValue(ctx)
	if err != nil {
		diags.AddError("Converting dynamic value", err.Error())
		return tftypes.NewValue(tftypes.DynamicPseudoType, nil)
	}
	return v
}

// jsonToDynamic turns raw JSON from the program into a dynamic attribute
// value, shaped after hint where possible (see jsonToValue).
func jsonToDynamic(ctx context.Context, raw []byte, hint tftypes.Type, diags *diag.Diagnostics) types.Dynamic {
	decoded, err := decodeJSON(raw)
	if err != nil {
		diags.AddError("Program returned invalid JSON", err.Error())
		return types.DynamicNull()
	}
	return tfToDynamic(ctx, jsonToValue(decoded, hint), diags)
}

func tfToDynamic(ctx context.Context, tfv tftypes.Value, diags *diag.Diagnostics) types.Dynamic {
	v, err := basetypes.DynamicType{}.ValueFromTerraform(ctx, tfv)
	if err != nil {
		diags.AddError("Converting program output", err.Error())
		return types.DynamicNull()
	}
	d, ok := v.(types.Dynamic)
	if !ok {
		diags.AddError("Converting program output", fmt.Sprintf("unexpected value type %T", v))
		return types.DynamicNull()
	}
	return d
}

func idPtr(s types.String) *string {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	v := s.ValueString()
	return &v
}

func boolPtr(b bool) *bool { return &b }

// privateReader is satisfied by the request/response private state holders.
type privateReader interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

type privateWriter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// readPrivate returns the program's private state, decoded, or nil.
func readPrivate(ctx context.Context, p privateReader, diags *diag.Diagnostics) any {
	if p == nil {
		return nil
	}
	raw, d := p.GetKey(ctx, privateKey)
	diags.Append(d...)
	if len(raw) == 0 {
		return nil
	}
	v, err := decodeJSON(raw)
	if err != nil {
		diags.AddWarning("Ignoring unreadable private state", err.Error())
		return nil
	}
	return v
}

// writePrivate stores the private state the program returned, if any. An
// absent key keeps what was there; an explicit null clears it.
func writePrivate(ctx context.Context, p privateWriter, resp *response, diags *diag.Diagnostics) {
	if p == nil || resp == nil || resp.Private == nil {
		return
	}
	if string(resp.Private) == "null" {
		diags.Append(p.SetKey(ctx, privateKey, nil)...)
		return
	}
	diags.Append(p.SetKey(ctx, privateKey, resp.Private)...)
}

// applyOutput decides the output after create or update: what the program
// returned, checked against anything the plan hook promised, or the planned
// value when the program said nothing.
func applyOutput(ctx context.Context, out *response, planned types.Dynamic, op string, diags *diag.Diagnostics) types.Dynamic {
	if out == nil || out.Output == nil {
		if planned.IsUnknown() {
			return types.DynamicNull()
		}
		return planned
	}
	// Deliberately no type hint: either the plan left output unknown, so any
	// shape is consistent with it, or it is checked for equality just below.
	got := jsonToDynamic(ctx, out.Output, nil, diags)
	if !planned.IsUnknown() && !planned.IsNull() {
		if !jsonEqual(dynamicToTF(ctx, planned, diags), dynamicToTF(ctx, got, diags)) {
			diags.AddError("Program output differs from what its plan hook promised",
				fmt.Sprintf("The plan hook set output for this %s, but the program returned a different value. "+
					"Either return the same output from both, or leave output out of the plan response.", op))
		}
	}
	return got
}

// applyProgramOverrides lets import and move responses fill in where the
// program lives and the resource's context, so the resulting state matches
// the configuration and the read that follows has what it needs.
func applyProgramOverrides(ctx context.Context, m *resourceModel, out *response, diags *diag.Diagnostics) {
	if out == nil {
		return
	}
	if len(out.Program) > 0 {
		l, d := types.ListValueFrom(ctx, types.StringType, out.Program)
		diags.Append(d...)
		m.Program = l
	}
	if out.WorkingDir != nil {
		m.WorkingDir = types.StringValue(*out.WorkingDir)
	}
	if out.Environment != nil {
		e, d := types.MapValueFrom(ctx, types.StringType, out.Environment)
		diags.Append(d...)
		m.Environment = e
	}
	if out.Context != nil {
		m.Context = jsonToDynamic(ctx, out.Context, nil, diags)
	}
}

func hookEnabled(b types.Bool) bool {
	return b.IsNull() || b.IsUnknown() || b.ValueBool()
}

// changedAttributes lists the top-level attributes whose planned value
// differs from state, in a fixed order, so that a replacement requested by
// the program can be attached to something Terraform agrees has changed.
func changedAttributes(plan, state tftypes.Value) []string {
	var planAttrs, stateAttrs map[string]tftypes.Value
	if err := plan.As(&planAttrs); err != nil {
		return nil
	}
	if err := state.As(&stateAttrs); err != nil {
		return nil
	}
	var changed []string
	for _, name := range []string{"input", "context", "program", "working_dir", "environment", "sensitive_environment", "timeout", "always_update", "plan_hook"} {
		if !planAttrs[name].Equal(stateAttrs[name]) {
			changed = append(changed, name)
		}
	}
	return changed
}

func (r *scriptedResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan resourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prog, ok := mustResolve(ctx, plan.programSettings, r.provider, &resp.Diagnostics)
	if !ok {
		return
	}
	in, _ := valueToJSON(dynamicToTF(ctx, plan.Input, &resp.Diagnostics))
	// Private state from the plan hook is not available here: the framework
	// starts create with empty private data. Documented in the protocol.
	out, result := prog.do(ctx, request{Op: "create", Input: in}, &resp.Diagnostics, resp.Private, true)
	if result != doOK {
		return
	}
	if out.ID == nil || *out.ID == "" {
		resp.Diagnostics.AddError("Create returned no id",
			"The program must print a JSON object with a non-empty \"id\" on create. If the object was "+
				"created anyway, it is now unmanaged: import it, or have create adopt an existing object.")
		return
	}
	plan.ID = types.StringValue(*out.ID)
	plan.Output = applyOutput(ctx, out, plan.Output, "create", &resp.Diagnostics)
	setIdentity(ctx, resp.Identity, plan.ID, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *scriptedResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state resourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	setIdentity(ctx, resp.Identity, state.ID, &resp.Diagnostics)
	pending, d := req.Private.GetKey(ctx, pendingMoveKey)
	resp.Diagnostics.Append(d...)

	prog, err := state.resolve(ctx, r.provider, &resp.Diagnostics)
	if err != nil {
		// State that came through a moved block or import may have no
		// program yet; the first apply fills it in and the next refresh runs.
		tflog.Debug(ctx, "read skipped: "+err.Error())
		if len(pending) > 0 {
			// The program gets its look at a moved block's source only on
			// the read right after the move; later, configuration has
			// already taken over and the id-only default stands.
			resp.Diagnostics.Append(resp.Private.SetKey(ctx, pendingMoveKey, nil)...)
		}
		return
	}
	if len(pending) > 0 {
		// A moved block brought this state over before the provider was
		// configured; give the program its look at the source now.
		if !r.completePendingMove(ctx, prog, &state, pending, resp.Private, &resp.Diagnostics) {
			return
		}
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, pendingMoveKey, nil)...)
		if resp.Diagnostics.HasError() {
			return
		}
		setIdentity(ctx, resp.Identity, state.ID, &resp.Diagnostics)
		if prog, err = state.resolve(ctx, r.provider, &resp.Diagnostics); err != nil {
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}

	priorInput := dynamicToTF(ctx, state.Input, &resp.Diagnostics)
	priorOutput := dynamicToTF(ctx, state.Output, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	in, _ := valueToJSON(priorInput)
	po, _ := valueToJSON(priorOutput)
	rq := request{Op: "read", ID: idPtr(state.ID), Input: in, PriorInput: in, PriorOutput: po,
		Private: readPrivate(ctx, req.Private, &resp.Diagnostics)}
	out, result := prog.do(ctx, rq, &resp.Diagnostics, resp.Private, false)
	if result != doOK {
		return // declined: trust the state
	}
	if out.Exists != nil && !*out.Exists {
		// The identity stays as stored: Terraform treats any change to it
		// during a read as a provider error, removal included.
		resp.State.RemoveResource(ctx)
		return
	}
	if out.ID != nil && *out.ID != "" {
		state.ID = types.StringValue(*out.ID)
	}
	if out.hasInput() {
		state.Input = jsonToDynamic(ctx, out.Input, priorInput.Type(), &resp.Diagnostics)
	}
	if out.Output != nil {
		state.Output = jsonToDynamic(ctx, out.Output, priorOutput.Type(), &resp.Diagnostics)
	}
	setIdentity(ctx, resp.Identity, state.ID, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *scriptedResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state resourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	planInput := dynamicToTF(ctx, plan.Input, &resp.Diagnostics)
	priorInput := dynamicToTF(ctx, state.Input, &resp.Diagnostics)
	priorOutput := dynamicToTF(ctx, state.Output, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	plan.ID = state.ID
	inputChanged := !jsonEqual(priorInput, planInput)
	if !inputChanged && !plan.AlwaysUpdate.ValueBool() {
		// Only program, environment or the like changed: record the new
		// settings without touching the managed resource.
		tflog.Debug(ctx, "update: input unchanged, program not run")
		plan.Output = state.Output
		setIdentity(ctx, resp.Identity, plan.ID, &resp.Diagnostics)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	prog, ok := mustResolve(ctx, plan.programSettings, r.provider, &resp.Diagnostics)
	if !ok {
		return
	}
	in, _ := valueToJSON(planInput)
	pi, _ := valueToJSON(priorInput)
	po, _ := valueToJSON(priorOutput)
	rq := request{Op: "update", ID: idPtr(state.ID), Input: in, PriorInput: pi, PriorOutput: po,
		InputChanged: boolPtr(inputChanged), Private: readPrivate(ctx, req.Private, &resp.Diagnostics)}
	out, result := prog.do(ctx, rq, &resp.Diagnostics, resp.Private, true)
	if result != doOK {
		return
	}
	if out.ID != nil && *out.ID != "" && *out.ID != state.ID.ValueString() {
		resp.Diagnostics.AddError("Update changed the id",
			fmt.Sprintf("The program returned id %q but the resource has id %q. An id cannot change on update; "+
				"ask for replacement from the plan hook instead.", *out.ID, state.ID.ValueString()))
		return
	}
	if out.Output == nil && plan.Output.IsUnknown() {
		plan.Output = state.Output
	} else {
		plan.Output = applyOutput(ctx, out, plan.Output, "update", &resp.Diagnostics)
	}
	setIdentity(ctx, resp.Identity, plan.ID, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *scriptedResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state resourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prog, err := state.resolve(ctx, r.provider, &resp.Diagnostics)
	if err != nil {
		if !resp.Diagnostics.HasError() {
			resp.Diagnostics.AddError("Cannot delete without a program",
				"This resource has no usable program ("+err.Error()+") and the provider sets none. Apply once "+
					"first, set `program` on the provider, or remove it from state with `terraform state rm`.")
		}
		return
	}
	in, _ := valueToJSON(dynamicToTF(ctx, state.Input, &resp.Diagnostics))
	po, _ := valueToJSON(dynamicToTF(ctx, state.Output, &resp.Diagnostics))
	rq := request{Op: "delete", ID: idPtr(state.ID), Input: in, PriorInput: in, PriorOutput: po,
		Private: readPrivate(ctx, req.Private, &resp.Diagnostics)}
	prog.do(ctx, rq, &resp.Diagnostics, nil, true)
}

// importID is the structured form of an import id, for when the program is
// not configured on the provider or differs from it.
type importID struct {
	ID          string            `json:"id"`
	Program     []string          `json:"program"`
	WorkingDir  string            `json:"working_dir"`
	Environment map[string]string `json:"environment"`
	Context     json.RawMessage   `json:"context"`
}

// ImportState accepts an identity (`import { identity = { id = ... } }`), a
// plain id, or a JSON object {id, program, working_dir, environment,
// context}. The program (from the JSON or the provider) is asked with op
// "import" to produce the initial state; a not_implemented answer keeps just
// the id, and the read that follows fills in the rest.
func (r *scriptedResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	model := emptyModel()
	rawID := req.ID
	if req.Identity != nil && !req.Identity.Raw.IsNull() {
		var identity identityModel
		resp.Diagnostics.Append(req.Identity.Get(ctx, &identity)...)
		if resp.Diagnostics.HasError() {
			return
		}
		rawID = identity.ID.ValueString()
	}
	var structured importID
	if err := json.Unmarshal([]byte(rawID), &structured); err == nil && structured.ID != "" {
		rawID = structured.ID
		if len(structured.Program) > 0 {
			l, d := types.ListValueFrom(ctx, types.StringType, structured.Program)
			resp.Diagnostics.Append(d...)
			model.Program = l
		}
		if structured.WorkingDir != "" {
			model.WorkingDir = types.StringValue(structured.WorkingDir)
		}
		if structured.Environment != nil {
			e, d := types.MapValueFrom(ctx, types.StringType, structured.Environment)
			resp.Diagnostics.Append(d...)
			model.Environment = e
		}
		if structured.Context != nil {
			model.Context = jsonToDynamic(ctx, structured.Context, nil, &resp.Diagnostics)
		}
	}
	model.ID = types.StringValue(rawID)
	if resp.Diagnostics.HasError() {
		return
	}

	prog, err := model.resolve(ctx, r.provider, &resp.Diagnostics)
	if err != nil {
		if !resp.Diagnostics.HasError() {
			resp.Diagnostics.AddError("Import needs a program",
				"Set `program` on the provider block, or import with a JSON id such as "+
					`{"id": "...", "program": ["python3", "manage.py"]}.`)
		}
		return
	}
	out, result := prog.do(ctx, request{Op: "import", ID: &rawID}, &resp.Diagnostics, resp.Private, false)
	switch result {
	case doFailed:
		return
	case doOK:
		if out.ID != nil && *out.ID != "" {
			model.ID = types.StringValue(*out.ID)
		}
		if out.hasInput() {
			model.Input = jsonToDynamic(ctx, out.Input, nil, &resp.Diagnostics)
		}
		if out.Output != nil {
			model.Output = jsonToDynamic(ctx, out.Output, nil, &resp.Diagnostics)
		}
		applyProgramOverrides(ctx, &model, out, &resp.Diagnostics)
	}
	setIdentity(ctx, resp.Identity, model.ID, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
