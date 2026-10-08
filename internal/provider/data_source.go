// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*scriptedDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*scriptedDataSource)(nil)
)

type scriptedDataSource struct {
	provider *providerData
}

type dataSourceModel struct {
	programSettings
	Input  types.Dynamic `tfsdk:"input"`
	Output types.Dynamic `tfsdk:"output"`
}

func (d *scriptedDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_data"
}

func (d *scriptedDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Read something with a program you write. The program is run with " +
			"`op = \"data\"` and the configured `input` on every refresh, and whatever it returns " +
			"under `output` becomes the `output` attribute. The same program as a `scripted_resource` " +
			"can serve, answering `data` next to its other operations.",
		Attributes: map[string]schema.Attribute{
			"program": schema.ListAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "Command and arguments to run. Defaults to the provider's `program`.",
				Validators:          []validator.List{listvalidator.SizeAtLeast(1)},
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
				ElementType:         types.StringType,
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Like `environment`, but hidden in plan output.",
			},
			"timeout": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "How long the program may take, as a duration such as `\"1m\"`. Defaults to the provider's `timeout`, else 10 minutes.",
			},
			"input": schema.DynamicAttribute{
				Optional:            true,
				MarkdownDescription: "What to read, in whatever shape the program understands. Sent as `input`.",
			},
			"context": schema.DynamicAttribute{
				Optional:            true,
				MarkdownDescription: "Further values for the program, sent as `context`.",
			},
			"output": schema.DynamicAttribute{
				Computed:            true,
				MarkdownDescription: "Whatever the program returned under `output`.",
			},
		},
	}
}

func (d *scriptedDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("Expected *providerData, got %T. This is a bug in the provider.", req.ProviderData))
		return
	}
	d.provider = data
}

func (d *scriptedDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg dataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	prog, err := cfg.resolve(ctx, d.provider, &resp.Diagnostics)
	var notYet errUnknown
	switch {
	case errors.As(err, &notYet):
		// Terraform defers the read until the values are known.
		cfg.Output = types.DynamicUnknown()
		resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
		return
	case errors.Is(err, errMissingProgram):
		resp.Diagnostics.AddError("No program to run", "Set `program` on the data source or on the provider block.")
		return
	case err != nil:
		return
	}
	in, unknown := valueToJSON(dynamicToTF(ctx, cfg.Input, &resp.Diagnostics))
	if len(unknown) > 0 {
		cfg.Output = types.DynamicUnknown()
		resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
		return
	}
	out, result := prog.do(ctx, request{Op: "data", Input: in}, &resp.Diagnostics, nil, true)
	if result != doOK {
		return
	}
	cfg.Output = types.DynamicNull()
	if out.Output != nil {
		cfg.Output = jsonToDynamic(ctx, out.Output, nil, &resp.Diagnostics)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
