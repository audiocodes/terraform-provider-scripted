// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// fakePrivate stands in for the framework's private state holder.
type fakePrivate struct {
	data map[string][]byte
}

func (f *fakePrivate) GetKey(_ context.Context, key string) ([]byte, diag.Diagnostics) {
	return f.data[key], nil
}

func (f *fakePrivate) SetKey(_ context.Context, key string, value []byte) diag.Diagnostics {
	if f.data == nil {
		f.data = map[string][]byte{}
	}
	if value == nil {
		delete(f.data, key)
	} else {
		f.data[key] = value
	}
	return nil
}

func TestPrivateStateHelpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var diags diag.Diagnostics
	if readPrivate(ctx, nil, &diags) != nil {
		t.Error("nil holder should read as nil")
	}
	p := &fakePrivate{data: map[string][]byte{privateKey: []byte(`{"a": 1}`)}}
	got, _ := readPrivate(ctx, p, &diags).(map[string]any)
	if got["a"] != json.Number("1") {
		t.Errorf("read: %v", got)
	}
	// An absent key keeps what is there; an explicit null clears it.
	writePrivate(ctx, p, &response{}, &diags)
	if _, ok := p.data[privateKey]; !ok {
		t.Error("absent key should keep the stored value")
	}
	writePrivate(ctx, p, &response{Private: json.RawMessage(`null`)}, &diags)
	if _, ok := p.data[privateKey]; ok {
		t.Error("explicit null should clear the stored value")
	}
	writePrivate(ctx, p, &response{Private: json.RawMessage(`{"b": 2}`)}, &diags)
	if string(p.data[privateKey]) != `{"b": 2}` {
		t.Errorf("write: %s", p.data[privateKey])
	}
	writePrivate(ctx, nil, &response{Private: json.RawMessage(`1`)}, &diags)
	p.data[privateKey] = []byte(`{not json`)
	if readPrivate(ctx, p, &diags) != nil || diags.WarningsCount() != 1 {
		t.Errorf("unreadable private state should warn and read as nil: %v", diags)
	}
}

func TestApplyOutput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var diags diag.Diagnostics
	promised := jsonToDynamic(ctx, []byte(`{"a": 1}`), nil, &diags)

	// Nothing returned: the promise stands, or null when nothing was promised.
	if got := applyOutput(ctx, nil, promised, "create", &diags); !got.Equal(promised) {
		t.Errorf("nil response should keep the planned output: %v", got)
	}
	if got := applyOutput(ctx, &response{}, types.DynamicUnknown(), "create", &diags); !got.IsNull() {
		t.Errorf("nil output with unknown plan should be null: %v", got)
	}
	// The same value back, in a different but equal shape, is fine.
	if applyOutput(ctx, &response{Output: json.RawMessage(`{"a": 1.0}`)}, promised, "create", &diags); diags.HasError() {
		t.Errorf("equal output should not error: %v", diags)
	}
	// A different value is an error.
	applyOutput(ctx, &response{Output: json.RawMessage(`{"a": 2}`)}, promised, "update", &diags)
	if !diags.HasError() || !strings.Contains(diags.Errors()[0].Summary(), "differs from what its plan hook promised") {
		t.Errorf("expected a mismatch error: %v", diags)
	}
	var d2 diag.Diagnostics
	if got := applyOutput(ctx, &response{Output: json.RawMessage(`[1, 2]`)}, types.DynamicUnknown(), "create", &d2); d2.HasError() || got.IsNull() {
		t.Errorf("unknown plan accepts anything: %v %v", got, d2)
	}
	applyOutput(ctx, &response{Output: json.RawMessage(`{bad`)}, types.DynamicUnknown(), "create", &d2)
	if !d2.HasError() {
		t.Error("invalid JSON output should error")
	}
}

func TestApplyProgramOverrides(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var diags diag.Diagnostics
	m := emptyModel()
	applyProgramOverrides(ctx, &m, nil, &diags)
	if !m.Program.IsNull() {
		t.Error("nil response should change nothing")
	}
	wd := "/w"
	applyProgramOverrides(ctx, &m, &response{
		Program:     []string{"python3", "x.py"},
		WorkingDir:  &wd,
		Environment: map[string]string{"A": "1"},
		Context:     json.RawMessage(`{"stack": "s"}`),
	}, &diags)
	if diags.HasError() || m.Program.IsNull() || m.WorkingDir.ValueString() != "/w" || m.Environment.IsNull() || m.Context.IsNull() {
		t.Errorf("overrides not applied: %+v %v", m, diags)
	}
	var elems []string
	_ = m.Program.ElementsAs(ctx, &elems, false)
	if len(elems) != 2 || elems[1] != "x.py" {
		t.Errorf("program: %v", elems)
	}
}

func TestSmallHelpers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var diags diag.Diagnostics
	if idPtr(types.StringNull()) != nil || idPtr(types.StringUnknown()) != nil || *idPtr(types.StringValue("x")) != "x" {
		t.Error("idPtr")
	}
	if !hookEnabled(types.BoolNull()) || !hookEnabled(types.BoolUnknown()) || hookEnabled(types.BoolValue(false)) {
		t.Error("hookEnabled")
	}
	if d := jsonToDynamic(ctx, []byte(`{`), nil, &diags); !d.IsNull() || !diags.HasError() {
		t.Error("jsonToDynamic should report invalid JSON")
	}
	var d2 diag.Diagnostics
	if d := jsonToDynamic(ctx, nil, nil, &d2); !d.IsNull() || d2.HasError() {
		t.Error("empty JSON is null")
	}
	if v := dynamicToTF(ctx, types.DynamicNull(), &d2); !v.IsNull() || d2.HasError() {
		t.Error("dynamicToTF null")
	}
	if got := exitStatus(nil); got != "exit status 0" {
		t.Errorf("exitStatus(nil) = %q", got)
	}
	var d3 diag.Diagnostics
	if _, ok := mustResolve(ctx, nullProgramSettings(), &providerData{}, &d3); ok || d3.ErrorsCount() != 1 {
		t.Error("mustResolve without any program should add one error")
	}
	if t0 := parseTimeout(types.StringNull(), &d3); t0 != 0 {
		t.Error("null timeout is zero")
	}
	if parseTimeout(types.StringValue("-1s"), &d3); d3.ErrorsCount() != 2 {
		t.Error("negative timeout should error")
	}
	for _, zero := range []string{"0", "0s", "0m"} {
		if got := parseTimeout(types.StringValue(zero), &d3); got != noTimeout || d3.ErrorsCount() != 2 {
			t.Errorf("timeout %q should mean no limit, got %s", zero, got)
		}
	}
}

func TestChangedAttributes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var diags diag.Diagnostics
	a := emptyModel()
	a.Input = jsonToDynamic(ctx, []byte(`{"x": 1}`), nil, &diags)
	b := a
	b.Input = jsonToDynamic(ctx, []byte(`{"x": 2}`), nil, &diags)
	b.Context = jsonToDynamic(ctx, []byte(`{"c": 1}`), nil, &diags)
	b.WorkingDir = types.StringValue("/w")
	// Build raw object values through the framework's own conversion.
	av, bv := modelToRaw(t, a), modelToRaw(t, b)
	got := changedAttributes(bv, av)
	want := []string{"input", "context", "working_dir"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("changed = %v, want %v", got, want)
	}
	if changedAttributes(av, av) != nil {
		t.Error("identical values should report no change")
	}
}

// modelToRaw converts a model to the tftypes object the framework would hold
// in plan or state.
func modelToRaw(t *testing.T, m resourceModel) tftypes.Value {
	t.Helper()
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	(&scriptedResource{}).Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	st := tfsdk.State{Schema: schemaResp.Schema, Raw: tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil)}
	if diags := st.Set(ctx, &m); diags.HasError() {
		t.Fatalf("setting model: %v", diags)
	}
	return st.Raw
}
