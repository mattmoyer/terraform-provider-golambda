// Package provider implements the golambda OpenTofu provider.
package provider

import (
	"context"
	"os"
	"path/filepath"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mattmoyer/terraform-provider-golambda/internal/toolchain"
)

// Address is the provider's registry address.
const Address = "spacelift.io/moyer/golambda"

// DefaultGoVersion is used when a resource doesn't set go_version.
const DefaultGoVersion = "1.27.4"

// New returns a provider factory.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &golambdaProvider{version: version} }
}

type golambdaProvider struct {
	version string
}

type providerModel struct {
	GitHubToken types.String `tfsdk:"github_token"`
	GoPrivate   types.List   `tfsdk:"goprivate"`
	CacheDir    types.String `tfsdk:"cache_dir"`
}

// clientConfig is shared by all resources of a configured provider.
type clientConfig struct {
	token     string
	goPrivate []string // nil means "derive from each target"
	cacheDir  string
	fetcher   *toolchain.Fetcher
}

func (p *golambdaProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "golambda"
	resp.Version = p.version
}

func (p *golambdaProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Builds reproducible AWS Lambda zip archives from Go packages in remote modules.",
		Attributes: map[string]schema.Attribute{
			"github_token": schema.StringAttribute{
				MarkdownDescription: "GitHub token used to fetch private modules from github.com. Defaults to the `GITHUB_TOKEN` environment variable.",
				Optional:            true,
				Sensitive:           true,
			},
			"goprivate": schema.ListAttribute{
				MarkdownDescription: "`GOPRIVATE` patterns: modules fetched directly from their origin, skipping the public proxy and checksum database. " +
					"Defaults to `github.com/<owner>` of each target when a GitHub token is available.",
				ElementType: types.StringType,
				Optional:    true,
			},
			"cache_dir": schema.StringAttribute{
				MarkdownDescription: "Directory for downloaded toolchains, Go module and build caches, and default output archives. Defaults to `golambda` under the user cache directory.",
				Optional:            true,
			},
		},
	}
}

func (p *golambdaProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var m providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cfg := &clientConfig{token: os.Getenv("GITHUB_TOKEN")}
	if !m.GitHubToken.IsNull() && !m.GitHubToken.IsUnknown() {
		cfg.token = m.GitHubToken.ValueString()
	}
	if !m.GoPrivate.IsNull() && !m.GoPrivate.IsUnknown() {
		cfg.goPrivate = []string{}
		resp.Diagnostics.Append(m.GoPrivate.ElementsAs(ctx, &cfg.goPrivate, false)...)
	}
	if !m.CacheDir.IsNull() && !m.CacheDir.IsUnknown() && m.CacheDir.ValueString() != "" {
		cfg.cacheDir = m.CacheDir.ValueString()
	} else {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		cfg.cacheDir = filepath.Join(base, "golambda")
	}
	abs, err := filepath.Abs(cfg.cacheDir)
	if err != nil {
		resp.Diagnostics.AddError("Invalid cache_dir", err.Error())
		return
	}
	cfg.cacheDir = abs
	cfg.fetcher = &toolchain.Fetcher{
		CacheDir: abs,
		Verifier: toolchain.NewSumDB(abs, nil),
	}
	resp.ResourceData = cfg
	resp.DataSourceData = cfg
}

func (p *golambdaProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{newZipArchiveResource}
}

func (p *golambdaProvider) DataSources(context.Context) []func() datasource.DataSource {
	return nil
}
