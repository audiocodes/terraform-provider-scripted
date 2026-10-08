// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ provider.Provider = (*scriptedProvider)(nil)
var _ provider.ProviderWithFunctions = (*scriptedProvider)(nil)

// New returns the provider constructor used by main and by the tests.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &scriptedProvider{version: version}
	}
}

type scriptedProvider struct {
	version string
	// data is what Configure produced. Resources get it through their own
	// Configure for most operations, but not for MoveResourceState, so the
	// resource factory hands it over directly as well.
	data *providerData
}

type providerModel struct {
	Program            types.List    `tfsdk:"program"`
	WorkingDir         types.String  `tfsdk:"working_dir"`
	Environment        types.Map     `tfsdk:"environment"`
	InheritEnvironment types.Bool    `tfsdk:"inherit_environment"`
	Input              types.Dynamic `tfsdk:"input"`
	Timeout            types.String  `tfsdk:"timeout"`
}

// providerData is handed to every resource after Configure.
type providerData struct {
	// program is the command resources run when they set none of their own.
	// It is also what import and moved blocks use, since at that point no
	// resource configuration is available. nil when not configured.
	program []string
	// programUnknown is set when the provider's program is not known yet,
	// which happens when it is derived from values decided during apply.
	programUnknown bool
	workingDir     string
	// environment is merged into the environment of every program, below
	// the resource's own environment settings.
	environment        map[string]string
	inheritEnvironment bool
	// input is sent to every program as provider_input; inputUnknown is set
	// when it is not decided until apply.
	input        any
	inputUnknown bool
	// timeout applies to every request that sets none of its own.
	timeout time.Duration
}

func (p *scriptedProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "scripted"
	resp.Version = p.version
}

func (p *scriptedProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage any resource with a program you write: the provider hands each " +
			"lifecycle operation to the program as JSON on stdin and records what it prints. " +
			"See the `scripted_resource` documentation for the protocol.\n\n" +
			"**Planning runs the program.** `plan` and `read` execute during `terraform plan`, " +
			"so merely planning a configuration runs whatever `program` points at, with Terraform's " +
			"credentials in its environment. Treat `program` like any other code your CI executes.\n\n" +
			"The program is started once per provider instance and kept running for its life (Terraform " +
			"starts an instance per graph walk, so two or three times in one `apply`); it reads one request " +
			"per line from stdin and answers one line per request. See the `scripted_resource` documentation " +
			"for the protocol.",
		Attributes: map[string]schema.Attribute{
			"program": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Command and arguments run for every resource that does not set its own " +
					"`program`, for example `[\"python3\", \"${path.root}/scripts/manage.py\"]`. Required " +
					"for `terraform import` and for `moved` blocks from other resource types, which have " +
					"no resource configuration to take a program from. A command without a path separator " +
					"is looked up in `PATH`; a relative path is resolved against `working_dir`.",
				Validators: []validator.List{listvalidator.SizeAtLeast(1)},
			},
			"working_dir": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Directory the program runs in when the resource sets no `working_dir`.",
			},
			"environment": schema.MapAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Sensitive:   true,
				MarkdownDescription: "Environment variables added to every program run by this provider. " +
					"A resource's own `environment` and `sensitive_environment` take precedence. " +
					"Values are never stored in state.",
			},
			"inherit_environment": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Pass Terraform's own environment (cloud credentials, `TF_VAR_*`, ...) on to " +
					"the program. Defaults to true. When false the program sees only `PATH`, `HOME` and a few " +
					"locale variables plus what `environment` sets.",
			},
			"input": schema.DynamicAttribute{
				Optional: true,
				MarkdownDescription: "Values every program run by this provider receives as `provider_input`, " +
					"in whatever shape suits it: server addresses, credentials, defaults. Unlike a " +
					"resource's `input` it is not managed state, and like all provider configuration it " +
					"is never stored in state.",
			},
			"timeout": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "How long the program may take to answer one request, as a duration such as " +
					"`\"30m\"`, for resources that set no `timeout` of their own; 10 minutes when unset, no limit at all " +
					"when `\"0\"`. A request not answered in time fails on its own; a program that stops answering " +
					"altogether is stopped.",
			},
		},
	}
}

func (p *scriptedProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	data := &providerData{environment: map[string]string{}, inheritEnvironment: true}
	switch {
	case cfg.Program.IsUnknown():
		data.programUnknown = true
	case !cfg.Program.IsNull():
		resp.Diagnostics.Append(cfg.Program.ElementsAs(ctx, &data.program, false)...)
	}
	if !cfg.WorkingDir.IsNull() && !cfg.WorkingDir.IsUnknown() {
		data.workingDir = cfg.WorkingDir.ValueString()
	}
	if !cfg.Environment.IsNull() && !cfg.Environment.IsUnknown() {
		resp.Diagnostics.Append(cfg.Environment.ElementsAs(ctx, &data.environment, false)...)
	}
	if !cfg.InheritEnvironment.IsNull() && !cfg.InheritEnvironment.IsUnknown() {
		data.inheritEnvironment = cfg.InheritEnvironment.ValueBool()
	}
	if in, unknown := valueToJSON(dynamicToTF(ctx, cfg.Input, &resp.Diagnostics)); len(unknown) > 0 {
		data.inputUnknown = true
	} else {
		data.input = in
	}
	data.timeout = parseTimeout(cfg.Timeout, &resp.Diagnostics)
	p.data = data
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *scriptedProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		func() resource.Resource { return &scriptedResource{provider: p.data} },
	}
}

func (p *scriptedProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		func() datasource.DataSource { return &scriptedDataSource{provider: p.data} },
	}
}

func (p *scriptedProvider) Functions(_ context.Context) []func() function.Function {
	return nil
}
