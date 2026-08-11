// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the data source satisfies the framework interfaces it relies on.
var (
	_ datasource.DataSource                     = &dlpDetectionEngineDataSource{}
	_ datasource.DataSourceWithConfigure        = &dlpDetectionEngineDataSource{}
	_ datasource.DataSourceWithConfigValidators = &dlpDetectionEngineDataSource{}
)

// NewDlpDetectionEngineDataSource returns a new barndoor_dlp_detection_engine data source.
func NewDlpDetectionEngineDataSource() datasource.DataSource {
	return &dlpDetectionEngineDataSource{}
}

// dlpDetectionEngineDataSource looks up an existing Data Protection detection
// engine (a "Protection Profile" in the app) by id or name through the
// dlp-service tenant admin REST API, so pre-existing engines can be
// referenced (e.g. from `barndoor_dlp_enforcement_policy.detection_engine_ids`)
// without being managed by Terraform.
type dlpDetectionEngineDataSource struct {
	client *client.Client
}

// The data source state reuses dlpDetectionEngineResourceModel — both
// surfaces carry the same attributes, every one authoritative server-side.

func (d *dlpDetectionEngineDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dlp_detection_engine"
}

func (d *dlpDetectionEngineDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing Data Protection (DLP) detection engine — a **Protection " +
			"Profile** in the platform app — by `id` or by `name`. Use it to feed " +
			"`barndoor_dlp_enforcement_policy.detection_engine_ids` with an engine that is not managed by " +
			"this Terraform configuration, or as the lookup step before a `terraform import`.\n\n" +
			"Engine names are unique per organization **and** `provider_type`, so one UI Protection Profile " +
			"can be several engines sharing a name; a by-`name` lookup that matches more than one engine " +
			"fails and lists the candidates — set `provider_type` (or `id`) to disambiguate.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Detection engine UUID. Exactly one of `id` or `name` must be set.",
				Optional:            true,
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name of the detection engine (the Protection Profile name in " +
					"the app). Exactly one of `id` or `name` must be set. The comparison is exact after " +
					"trimming surrounding whitespace.",
				Optional: true,
				Computed: true,
			},
			"provider_type": schema.StringAttribute{
				MarkdownDescription: "Detection provider backing the engine (e.g. `presidio`, `google_dlp`). " +
					"Optional narrowing for a by-`name` lookup — the same name may exist for several " +
					"provider types; only meaningful together with `name`.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.AlsoRequires(path.MatchRoot("name")),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the detection engine belongs to.",
				Computed:            true,
			},
			"provider_connection_name": schema.StringAttribute{
				MarkdownDescription: "Name of the Data Protection provider connection the engine uses, if any.",
				Computed:            true,
			},
			"enabled_detection_types": schema.ListAttribute{
				MarkdownDescription: "Detection type wire names the engine scans for.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"config": schema.StringAttribute{
				MarkdownDescription: "Provider-specific engine configuration as JSON.",
				Computed:            true,
				CustomType:          jsontypes.NormalizedType{},
			},
			"supported_detection_types": schema.ListAttribute{
				MarkdownDescription: "Every detection type wire name the engine's provider can produce.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"supported_actions": schema.ListAttribute{
				MarkdownDescription: "Enforcement policy actions the engine supports.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"provider_class": schema.StringAttribute{
				MarkdownDescription: "Capability class of the provider: `native`, `span_detection`, or " +
					"`guardrail_intervention`.",
				Computed: true,
			},
			"capabilities": schema.SingleNestedAttribute{
				MarkdownDescription: "Capability flags of the engine's provider.",
				Computed:            true,
				Attributes:          dlpDetectionEngineCapabilitiesDataSourceAttributes(),
			},
			"runtime_stages": schema.ListAttribute{
				MarkdownDescription: "Runtime stages the engine can evaluate.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"created_by": schema.StringAttribute{
				MarkdownDescription: "Subject that created the engine.",
				Computed:            true,
			},
			"updated_by": schema.StringAttribute{
				MarkdownDescription: "Subject that last updated the engine.",
				Computed:            true,
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the engine was created (RFC 3339).",
				Computed:            true,
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the engine was last updated (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

// dlpDetectionEngineCapabilitiesDataSourceAttributes mirrors the resource's
// capabilities object in data-source schema types (all computed).
func dlpDetectionEngineCapabilitiesDataSourceAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"provider_class":                 schema.StringAttribute{Computed: true},
		"pii_detection":                  schema.BoolAttribute{Computed: true},
		"secret_detection":               schema.BoolAttribute{Computed: true},
		"prompt_attack_detection":        schema.BoolAttribute{Computed: true},
		"span_offsets":                   schema.BoolAttribute{Computed: true},
		"confidence_scores":              schema.BoolAttribute{Computed: true},
		"native_masking":                 schema.BoolAttribute{Computed: true},
		"native_blocking":                schema.BoolAttribute{Computed: true},
		"supports_tokenization_pipeline": schema.BoolAttribute{Computed: true},
	}
}

// ConfigValidators requires exactly one of the two lookup keys.
// provider_type additionally requires name (attribute validator above).
func (d *dlpDetectionEngineDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *dlpDetectionEngineDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		// ProviderData is nil during the framework's early lifecycle phases
		// (schema/validation), before the provider's Configure has populated it.
		return
	}

	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	d.client = c
}

func (d *dlpDetectionEngineDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data dlpDetectionEngineResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var engine dlpDetectionEngineResponse
	if id, ok := knownString(data.ID); ok {
		err := doJSON(ctx, d.client, http.MethodGet, dlpAPIPrefix+"/detection-engines/"+id, nil, &engine)
		if err != nil {
			if isNotFound(err) {
				resp.Diagnostics.AddError(
					"Detection engine not found",
					fmt.Sprintf("No Data Protection detection engine (Protection Profile) with id %q exists "+
						"in the organization. Confirm the id, or look the engine up by `name`.", id),
				)
				return
			}
			addDlpDetectionEngineAPIError(&resp.Diagnostics, "read the detection engine", err)
			return
		}
	} else {
		found, ok := d.resolveDetectionEngineByName(ctx, data.Name.ValueString(), data.ProviderType, &resp.Diagnostics)
		if !ok {
			return
		}
		engine = *found
	}

	// The prior model is all-null here (data sources have no prior state), so
	// empty optionals settle to null.
	state, err := applyDlpDetectionEngineResponse(ctx, &engine, &dlpDetectionEngineResourceModel{})
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the Data Protection API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// resolveDetectionEngineByName finds the single engine whose name matches
// exactly (after trimming surrounding whitespace), optionally narrowed by
// provider_type. The list endpoint returns the organization's full engine set
// as a bare array (no pagination); the exact comparison happens here because
// names alone are not a unique key — (name, provider_type) is.
func (d *dlpDetectionEngineDataSource) resolveDetectionEngineByName(ctx context.Context, name string, providerType types.String, diags *diag.Diagnostics) (*dlpDetectionEngineResponse, bool) {
	want := strings.TrimSpace(name)
	wantProviderType, narrowed := knownString(providerType)

	var engines []dlpDetectionEngineResponse
	if err := doJSON(ctx, d.client, http.MethodGet, dlpAPIPrefix+"/detection-engines", nil, &engines); err != nil {
		addDlpDetectionEngineAPIError(diags, "list detection engines by name", err)
		return nil, false
	}

	var matches []dlpDetectionEngineResponse
	for _, e := range engines {
		if strings.TrimSpace(e.Name) != want {
			continue
		}
		if narrowed && e.ProviderType != wantProviderType {
			continue
		}
		matches = append(matches, e)
	}

	switch len(matches) {
	case 0:
		qualifier := ""
		if narrowed {
			qualifier = fmt.Sprintf(" with provider_type %q", wantProviderType)
		}
		diags.AddError(
			"Detection engine not found",
			fmt.Sprintf("No Data Protection detection engine (Protection Profile) named %q%s exists in the "+
				"organization. The comparison is exact (case-sensitive); confirm the name, or look the "+
				"engine up by `id`.", want, qualifier),
		)
		return nil, false
	case 1:
		return &matches[0], true
	default:
		candidates := make([]string, 0, len(matches))
		for _, m := range matches {
			candidates = append(candidates, fmt.Sprintf("%s (provider_type %s)", m.ID, m.ProviderType))
		}
		diags.AddError(
			"Detection engine name is ambiguous",
			fmt.Sprintf("%d detection engines are named %q: %s. Names are only unique per provider type "+
				"(one Protection Profile in the app can span several engines); set `provider_type` or use "+
				"`id` to disambiguate.", len(matches), want, strings.Join(candidates, ", ")),
		)
		return nil, false
	}
}
