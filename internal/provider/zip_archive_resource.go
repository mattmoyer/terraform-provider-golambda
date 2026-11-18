package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/mattmoyer/terraform-provider-golambda/internal/archive"
	"github.com/mattmoyer/terraform-provider-golambda/internal/build"
	"github.com/mattmoyer/terraform-provider-golambda/internal/toolchain"
)

var (
	_ resource.Resource               = (*zipArchiveResource)(nil)
	_ resource.ResourceWithConfigure  = (*zipArchiveResource)(nil)
	_ resource.ResourceWithModifyPlan = (*zipArchiveResource)(nil)
)

func newZipArchiveResource() resource.Resource { return &zipArchiveResource{} }

type zipArchiveResource struct {
	cfg *clientConfig
}

type zipArchiveModel struct {
	ID                 types.String `tfsdk:"id"`
	Target             types.String `tfsdk:"target"`
	Args               types.List   `tfsdk:"args"`
	GoVersion          types.String `tfsdk:"go_version"`
	GOOS               types.String `tfsdk:"goos"`
	GOARCH             types.String `tfsdk:"goarch"`
	BuildTags          types.List   `tfsdk:"build_tags"`
	LDFlags            types.String `tfsdk:"ldflags"`
	OutputPath         types.String `tfsdk:"output_path"`
	OutputSHA256       types.String `tfsdk:"output_sha256"`
	OutputBase64SHA256 types.String `tfsdk:"output_base64sha256"`
	OutputSize         types.Int64  `tfsdk:"output_size"`
	ResolvedVersion    types.String `tfsdk:"resolved_version"`
}

func (r *zipArchiveResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_zip_archive"
}

var platformRE = regexp.MustCompile(`^[a-z0-9]+$`)

func (r *zipArchiveResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Builds a Go main package from a remote module into a reproducible AWS Lambda zip archive. " +
			"The build runs during plan, so build failures fail the plan and the archive hash is known before apply.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The archive's SHA-256 (hex).",
				Computed:            true,
			},
			"target": schema.StringAttribute{
				MarkdownDescription: "Package to build, as `<package>@<version>`, like `go install`. The version must be a commit hash or an exact semantic version.",
				Required:            true,
				Validators:          []validator.String{targetValidator{}},
			},
			"args": schema.ListAttribute{
				MarkdownDescription: "Arguments to run the binary with. When set, `bootstrap` is a shell script that execs the binary (stored under its own name) with these arguments.",
				ElementType:         types.StringType,
				Optional:            true,
				Validators:          []validator.List{listvalidator.ValueStringsAre(stringvalidator.RegexMatches(regexp.MustCompile(`^[^\x00]*$`), "must not contain NUL bytes"))},
			},
			"go_version": schema.StringAttribute{
				MarkdownDescription: "Go toolchain version, like `1.27.4`. Defaults to `" + DefaultGoVersion + "`; the default may change in new provider releases.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(DefaultGoVersion),
				Validators:          []validator.String{goVersionValidator{}},
			},
			"goos": schema.StringAttribute{
				MarkdownDescription: "Target `GOOS`. Defaults to `linux`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("linux"),
				Validators:          []validator.String{stringvalidator.RegexMatches(platformRE, "must be a GOOS value")},
			},
			"goarch": schema.StringAttribute{
				MarkdownDescription: "Target `GOARCH`. Defaults to `arm64`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("arm64"),
				Validators:          []validator.String{stringvalidator.RegexMatches(platformRE, "must be a GOARCH value")},
			},
			"build_tags": schema.ListAttribute{
				MarkdownDescription: "Build tags. Defaults to `[\"lambda.norpc\"]`.",
				ElementType:         types.StringType,
				Optional:            true,
				Computed:            true,
				Default:             listdefault.StaticValue(types.ListValueMust(types.StringType, []attr.Value{types.StringValue("lambda.norpc")})),
			},
			"ldflags": schema.StringAttribute{
				MarkdownDescription: "Extra linker flags, appended to `" + build.FixedLDFlags + "`.",
				Optional:            true,
			},
			"output_path": schema.StringAttribute{
				MarkdownDescription: "Where to write the archive. Defaults to a path under the provider's `cache_dir` named after the archive's SHA-256, so it only changes when the contents do.",
				Optional:            true,
				Computed:            true,
			},
			"output_sha256": schema.StringAttribute{
				MarkdownDescription: "SHA-256 of the archive, hex encoded.",
				Computed:            true,
			},
			"output_base64sha256": schema.StringAttribute{
				MarkdownDescription: "SHA-256 of the archive, base64 encoded. Use this for `aws_lambda_function.source_code_hash`.",
				Computed:            true,
			},
			"output_size": schema.Int64Attribute{
				MarkdownDescription: "Size of the archive in bytes.",
				Computed:            true,
			},
			"resolved_version": schema.StringAttribute{
				MarkdownDescription: "The canonical module version the target's version resolved to, such as a pseudo-version for a commit hash.",
				Computed:            true,
			},
		},
	}
}

func (r *zipArchiveResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	cfg, ok := req.ProviderData.(*clientConfig)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	r.cfg = cfg
}

// ModifyPlan builds the archive during plan so failures surface before
// anyone approves an apply, and so the archive hash is known in the plan.
func (r *zipArchiveResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || r.cfg == nil {
		return // destroy, or provider not configured yet
	}
	var plan zipArchiveModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.inputsKnown() {
		// Depends on values that don't exist yet; build at apply.
		return
	}
	if !req.State.Raw.IsNull() {
		var state zipArchiveModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if plan.sameInputs(state, r.defaultOutputPath(state.OutputSHA256.ValueString())) {
			return // nothing to rebuild; computed values carry over from state
		}
	}

	art, diags := r.produce(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	art.apply(&plan)
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *zipArchiveResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan zipArchiveModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.buildForApply(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *zipArchiveResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan zipArchiveModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.buildForApply(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// buildForApply rebuilds (cheap with a warm Go build cache) and checks the
// result matches what the plan promised.
func (r *zipArchiveResource) buildForApply(ctx context.Context, plan *zipArchiveModel) diag.Diagnostics {
	art, diags := r.produce(ctx, *plan)
	if diags.HasError() {
		return diags
	}
	if want := plan.OutputSHA256; !want.IsUnknown() && !want.IsNull() && want.ValueString() != art.sha256 {
		diags.AddError("Non-reproducible build",
			fmt.Sprintf("Building %s for %s/%s with Go %s produced an archive with SHA-256 %s, but the plan expected %s. "+
				"The build is not reproducible across these runs (for example, the target's dependencies or the Go toolchain differ between the plan and apply environments). Re-run the plan.",
				plan.Target.ValueString(), plan.GOOS.ValueString(), plan.GOARCH.ValueString(), plan.GoVersion.ValueString(), art.sha256, want.ValueString()))
		return diags
	}
	art.apply(plan)
	return diags
}

// Read restores the archive file if it's missing (for example on a fresh
// worker), so dependents that read output_path find it.
func (r *zipArchiveResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state zipArchiveModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || r.cfg == nil {
		return
	}
	path := state.OutputPath.ValueString()
	if data, err := os.ReadFile(path); err == nil && sha256Hex(data) == state.OutputSHA256.ValueString() {
		return
	}
	tflog.Info(ctx, "archive missing or changed on disk; rebuilding", map[string]any{"path": path})

	// Keep a user-chosen path; a default path follows the (possibly new) hash.
	rebuild := state
	if path == r.defaultOutputPath(state.OutputSHA256.ValueString()) {
		rebuild.OutputPath = types.StringUnknown()
	}
	art, diags := r.produce(ctx, rebuild)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if art.sha256 != state.OutputSHA256.ValueString() {
		resp.Diagnostics.AddWarning("Archive changed on rebuild",
			fmt.Sprintf("Rebuilding %s produced SHA-256 %s instead of %s recorded in state. Dependent resources will see the new hash.",
				state.Target.ValueString(), art.sha256, state.OutputSHA256.ValueString()))
	}
	art.apply(&state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *zipArchiveResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
	// Nothing remote to delete. Output files and caches are left in place.
}

type artifact struct {
	path, sha256, base64sha256, resolvedVersion string
	size                                        int64
}

func (a artifact) apply(m *zipArchiveModel) {
	m.ID = types.StringValue(a.sha256)
	m.OutputPath = types.StringValue(a.path)
	m.OutputSHA256 = types.StringValue(a.sha256)
	m.OutputBase64SHA256 = types.StringValue(a.base64sha256)
	m.OutputSize = types.Int64Value(a.size)
	m.ResolvedVersion = types.StringValue(a.resolvedVersion)
}

// produce fetches the toolchain, builds the binary, and writes the archive.
func (r *zipArchiveResource) produce(ctx context.Context, m zipArchiveModel) (artifact, diag.Diagnostics) {
	var diags diag.Diagnostics
	target := m.Target.ValueString()
	pkg, _, err := build.ParseTarget(target)
	if err != nil {
		diags.AddAttributeError(pathTarget, "Invalid target", err.Error())
		return artifact{}, diags
	}
	var args, tags []string
	diags.Append(m.Args.ElementsAs(ctx, &args, false)...)
	diags.Append(m.BuildTags.ElementsAs(ctx, &tags, false)...)
	if diags.HasError() {
		return artifact{}, diags
	}

	start := time.Now()
	goRoot, err := r.cfg.fetcher.Ensure(ctx, m.GoVersion.ValueString())
	if err != nil {
		diags.AddError("Failed to install Go toolchain", err.Error())
		return artifact{}, diags
	}

	goPrivate := r.cfg.goPrivate
	if goPrivate == nil && r.cfg.token != "" {
		if p := build.DefaultGoPrivate(pkg); p != "" {
			goPrivate = []string{p}
		}
	}
	res, err := build.Build(ctx, build.Options{
		GoRoot:      goRoot,
		CacheDir:    r.cfg.cacheDir,
		Target:      target,
		GOOS:        m.GOOS.ValueString(),
		GOARCH:      m.GOARCH.ValueString(),
		Tags:        tags,
		LDFlags:     m.LDFlags.ValueString(),
		GitHubToken: r.cfg.token,
		GoPrivate:   goPrivate,
	})
	if err != nil {
		diags.AddError("Go build failed", err.Error())
		return artifact{}, diags
	}

	zipBytes, err := archive.Build(res.Binary, res.BinaryName, args)
	if err != nil {
		diags.AddError("Failed to create archive", err.Error())
		return artifact{}, diags
	}
	sum := sha256.Sum256(zipBytes)
	art := artifact{
		sha256:          hex.EncodeToString(sum[:]),
		base64sha256:    base64.StdEncoding.EncodeToString(sum[:]),
		size:            int64(len(zipBytes)),
		resolvedVersion: res.ResolvedVersion,
	}
	art.path = r.defaultOutputPath(art.sha256)
	if p := m.OutputPath; !p.IsUnknown() && !p.IsNull() && p.ValueString() != "" {
		art.path = p.ValueString()
	}
	if err := archive.WriteFile(art.path, zipBytes); err != nil {
		diags.AddError("Failed to write archive", err.Error())
		return artifact{}, diags
	}
	tflog.Info(ctx, "built lambda archive", map[string]any{
		"target": target, "resolved_version": res.ResolvedVersion, "sha256": art.sha256,
		"path": art.path, "duration": time.Since(start).String(),
	})
	return art, diags
}

func (r *zipArchiveResource) defaultOutputPath(sha string) string {
	if sha == "" {
		return ""
	}
	return filepath.Join(r.cfg.cacheDir, "out", fmt.Sprintf("v%d", archive.FormatVersion), sha+".zip")
}

func (m zipArchiveModel) inputsKnown() bool {
	return !m.Target.IsUnknown() && !m.Args.IsUnknown() && !m.GoVersion.IsUnknown() &&
		!m.GOOS.IsUnknown() && !m.GOARCH.IsUnknown() && !m.BuildTags.IsUnknown() &&
		!m.LDFlags.IsUnknown()
}

// sameInputs reports whether m would build the same archive already in state.
func (m zipArchiveModel) sameInputs(state zipArchiveModel, stateDefaultPath string) bool {
	if !m.Target.Equal(state.Target) || !m.Args.Equal(state.Args) || !m.GoVersion.Equal(state.GoVersion) ||
		!m.GOOS.Equal(state.GOOS) || !m.GOARCH.Equal(state.GOARCH) || !m.BuildTags.Equal(state.BuildTags) ||
		!m.LDFlags.Equal(state.LDFlags) {
		return false
	}
	if m.OutputPath.IsUnknown() {
		// Unset in config: unchanged only if state was using the default.
		return state.OutputPath.ValueString() == stateDefaultPath
	}
	return m.OutputPath.Equal(state.OutputPath)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type targetValidator struct{}

func (targetValidator) Description(context.Context) string {
	return "must be <package>@<version> with a commit hash or exact semantic version"
}
func (v targetValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (targetValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, _, err := build.ParseTarget(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid target", err.Error())
	}
}

type goVersionValidator struct{}

func (goVersionValidator) Description(context.Context) string {
	return "must be a Go release version like 1.27.4 (no \"go\" prefix)"
}
func (v goVersionValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (v goVersionValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s := req.ConfigValue.ValueString()
	if !toolchain.ValidVersion(strings.TrimPrefix(s, "go")) || strings.HasPrefix(s, "go") {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid go_version", fmt.Sprintf("%q %s", s, v.Description(ctx)))
	}
}

var pathTarget = path.Root("target")
