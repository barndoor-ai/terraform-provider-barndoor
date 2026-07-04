// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// dlpAPIPrefix is the dlp-service tenant admin API mount point under the
// platform host root (routed by the edge to dlp-service's versioned admin
// API). Requests are scoped to the credential's organization by the
// `internal_organization_id` claim of its Keycloak token.
const dlpAPIPrefix = "api/dlp/admin/v1"

// Platform defaults for the organization's DLP configuration, from
// dlp-service's `dlp.org_configs` table defaults (migration
// V001__policy_store.sql). Delete writes these back before forgetting the
// resource, so a destroyed configuration behaves as if it was never managed.
const (
	dlpOrgConfigDefaultEnabled      = true
	dlpOrgConfigDefaultGlobalDryRun = false
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &dlpOrgConfigResource{}
	_ resource.ResourceWithConfigure   = &dlpOrgConfigResource{}
	_ resource.ResourceWithImportState = &dlpOrgConfigResource{}
)

// NewDlpOrgConfigResource returns a new barndoor_dlp_org_config resource.
func NewDlpOrgConfigResource() resource.Resource {
	return &dlpOrgConfigResource{}
}

// dlpOrgConfigResource manages the organization's singleton Data Protection
// (DLP) configuration through the dlp-service tenant admin REST API
// (`/api/dlp/admin/v1/config`).
type dlpOrgConfigResource struct {
	client *client.Client
}

// dlpOrgConfigResourceModel maps the resource schema to Go types.
type dlpOrgConfigResourceModel struct {
	ID           types.String `tfsdk:"id"`
	OrgID        types.String `tfsdk:"org_id"`
	Enabled      types.Bool   `tfsdk:"enabled"`
	GlobalDryRun types.Bool   `tfsdk:"global_dry_run"`
	CreatedAt    types.String `tfsdk:"created_at"`
	UpdatedAt    types.String `tfsdk:"updated_at"`
}

func (r *dlpOrgConfigResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dlp_org_config"
}

func (r *dlpOrgConfigResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the organization's **singleton** Data Protection (DLP) configuration: " +
			"whether DLP enforcement is enabled and whether every policy runs in dry-run (observe-only) mode. " +
			"The platform provisions this configuration row per organization (reading it auto-creates it with " +
			"the platform defaults), so this resource **adopts and configures** it rather than creating " +
			"anything: attributes left unset keep their current server-side values.\n\n" +
			"`terraform destroy` cannot delete the row — it **resets both settings to the platform defaults** " +
			"(`enabled = " + fmt.Sprintf("%t", dlpOrgConfigDefaultEnabled) + "`, `global_dry_run = " +
			fmt.Sprintf("%t", dlpOrgConfigDefaultGlobalDryRun) + "`) and then forgets the resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "ID of the configuration; the API keys the singleton by the " +
					"organization, so this equals `org_id`.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"org_id": schema.StringAttribute{
				MarkdownDescription: "Organization the configuration belongs to, resolved from the provider " +
					"credential's token claims.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether Data Protection enforcement is enabled for the organization. " +
					"The platform default is `" + fmt.Sprintf("%t", dlpOrgConfigDefaultEnabled) + "`; when " +
					"unset, the current server-side value is kept and tracked.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"global_dry_run": schema.BoolAttribute{
				MarkdownDescription: "Whether every Data Protection policy runs in dry-run (observe-only) " +
					"mode regardless of its own `dry_run` flag. The platform default is `" +
					fmt.Sprintf("%t", dlpOrgConfigDefaultGlobalDryRun) + "`; when unset, the current " +
					"server-side value is kept and tracked.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "When the configuration row was created (RFC 3339).",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				MarkdownDescription: "When the configuration was last updated (RFC 3339).",
				Computed:            true,
			},
		},
	}
}

func (r *dlpOrgConfigResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		// Configure is called before the provider is configured (e.g. during
		// schema validation); nothing to wire up yet.
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
	r.client = c
}

// Create adopts the organization's configuration row: the API upserts on PUT
// (and auto-creates on first GET), so "create" is a configure of the existing
// singleton. Only configured attributes are sent; the server keeps its current
// value for anything omitted.
func (r *dlpOrgConfigResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpOrgConfigResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var cfg dlpOrgConfigResponse
	if err := doJSON(ctx, r.client, http.MethodPut, dlpAPIPrefix+"/config",
		buildDlpOrgConfigWriteRequest(&plan), &cfg); err != nil {
		addDlpOrgConfigAPIError(&resp.Diagnostics, "configure the Data Protection settings", err)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyDlpOrgConfigResponse(&cfg))...)
}

func (r *dlpOrgConfigResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state dlpOrgConfigResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var cfg dlpOrgConfigResponse
	if err := doJSON(ctx, r.client, http.MethodGet, dlpAPIPrefix+"/config", nil, &cfg); err != nil {
		if isNotFound(err) {
			// Should not happen (the GET auto-creates the row), but a 404 can
			// only mean the configuration is gone; plan a recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addDlpOrgConfigAPIError(&resp.Diagnostics, "read the Data Protection settings", err)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyDlpOrgConfigResponse(&cfg))...)
}

func (r *dlpOrgConfigResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan dlpOrgConfigResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var cfg dlpOrgConfigResponse
	if err := doJSON(ctx, r.client, http.MethodPut, dlpAPIPrefix+"/config",
		buildDlpOrgConfigWriteRequest(&plan), &cfg); err != nil {
		addDlpOrgConfigAPIError(&resp.Diagnostics, "update the Data Protection settings", err)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyDlpOrgConfigResponse(&cfg))...)
}

// Delete resets the singleton to the platform defaults (the row itself cannot
// be deleted — the API has no delete endpoint and every other DLP object
// references it), then lets Terraform forget the resource.
func (r *dlpOrgConfigResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	enabled := dlpOrgConfigDefaultEnabled
	globalDryRun := dlpOrgConfigDefaultGlobalDryRun
	body := &dlpOrgConfigWriteRequest{Enabled: &enabled, GlobalDryRun: &globalDryRun}
	if err := doJSON(ctx, r.client, http.MethodPut, dlpAPIPrefix+"/config", body, nil); err != nil {
		addDlpOrgConfigAPIError(&resp.Diagnostics,
			"reset the Data Protection settings to the platform defaults (destroy)", err)
	}
}

// ImportState imports the organization's singleton configuration. The API
// resolves the organization from the credential's token claims, so the import
// ID is not looked up — pass the organization ID (which the API also uses as
// the configuration's `id`) for consistency; Read replaces it with the
// server's authoritative value either way.
func (r *dlpOrgConfigResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *dlpOrgConfigResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// addDlpOrgConfigAPIError turns a DLP admin API error into an actionable
// diagnostic. action is the failed verb phrase, e.g. "read the Data
// Protection settings".
func addDlpOrgConfigAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		diags.AddError(
			"Permission denied by the Data Protection API",
			fmt.Sprintf("Failed to %s: the configured credential was not accepted. Confirm the "+
				"service-account credential's token carries the organization claim for the organization "+
				"whose Data Protection settings you are managing.\n\nServer message: %s",
				action, apiErr.displayBody()),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// --- request/response DTOs ---------------------------------------------------

// dlpOrgConfigWriteRequest mirrors the dlp-service UpdateOrgConfigRequest
// body. Omitted fields keep their current server-side values (the upsert
// COALESCEs them).
type dlpOrgConfigWriteRequest struct {
	Enabled      *bool `json:"enabled,omitempty"`
	GlobalDryRun *bool `json:"global_dry_run,omitempty"`
}

// dlpOrgConfigResponse mirrors the dlp-service OrgConfigResponse.
type dlpOrgConfigResponse struct {
	ID           string `json:"id"`
	OrgID        string `json:"org_id"`
	Enabled      bool   `json:"enabled"`
	GlobalDryRun bool   `json:"global_dry_run"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// buildDlpOrgConfigWriteRequest converts the planned model to the API body,
// sending only attributes that are known (set by config or carried from prior
// state) so the server's current values back-fill the rest.
func buildDlpOrgConfigWriteRequest(plan *dlpOrgConfigResourceModel) *dlpOrgConfigWriteRequest {
	body := &dlpOrgConfigWriteRequest{}
	if known(plan.Enabled) {
		v := plan.Enabled.ValueBool()
		body.Enabled = &v
	}
	if known(plan.GlobalDryRun) {
		v := plan.GlobalDryRun.ValueBool()
		body.GlobalDryRun = &v
	}
	return body
}

// applyDlpOrgConfigResponse maps the server's view onto a state model. Every
// attribute is authoritative server-side.
func applyDlpOrgConfigResponse(cfg *dlpOrgConfigResponse) *dlpOrgConfigResourceModel {
	return &dlpOrgConfigResourceModel{
		ID:           types.StringValue(cfg.ID),
		OrgID:        types.StringValue(cfg.OrgID),
		Enabled:      types.BoolValue(cfg.Enabled),
		GlobalDryRun: types.BoolValue(cfg.GlobalDryRun),
		CreatedAt:    types.StringValue(cfg.CreatedAt),
		UpdatedAt:    types.StringValue(cfg.UpdatedAt),
	}
}
