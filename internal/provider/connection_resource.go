// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &connectionResource{}
	_ resource.ResourceWithConfigure   = &connectionResource{}
	_ resource.ResourceWithImportState = &connectionResource{}
)

// NewConnectionResource returns a new barndoor_connection resource.
func NewConnectionResource() resource.Resource {
	return &connectionResource{}
}

// connectionResource manages the organization's tenant-wide (service-account-
// owned) credential connection to an MCP server, through the registry-service
// public REST API (`/api/registry/v1/servers/{id}/connect|connection` with
// `as_service=true`). Only non-OAuth credential providers are supported:
// OAuth connections require an interactive browser consent that a declarative
// apply cannot perform.
type connectionResource struct {
	client *client.Client
}

// connectionResourceModel maps the resource schema to Go types.
type connectionResourceModel struct {
	ID               types.String `tfsdk:"id"`
	ServerID         types.String `tfsdk:"server_id"`
	McpServerID      types.String `tfsdk:"mcp_server_id"`
	Status           types.String `tfsdk:"status"`
	APIKey           types.String `tfsdk:"api_key"`
	BearerToken      types.String `tfsdk:"bearer_token"`
	Username         types.String `tfsdk:"username"`
	Password         types.String `tfsdk:"password"`
	AdditionalFields types.Map    `tfsdk:"additional_fields"`
}

func (r *connectionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_connection"
}

func (r *connectionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the organization's **tenant-wide** (service-account-owned) credential " +
			"connection to an MCP server. An organization holds at most one such connection per server; it " +
			"backs calls that are not tied to an individual user's grant.\n\n" +
			"Only **non-OAuth** credential providers are supported (`api_key`, `bearer_token`, `basic_auth`, " +
			"`generic`) — these complete synchronously from the supplied credentials. OAuth providers require " +
			"an interactive browser consent with the upstream identity provider, which a declarative apply " +
			"cannot perform; connect those out-of-band (e.g. in the Barndoor app) instead.\n\n" +
			"Credential attributes are **write-only**: the platform stores them in its secret store and never " +
			"returns them, so Terraform tracks the configured values and cannot detect out-of-band changes. " +
			"Changing any credential replaces the connection.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Connection UUID assigned by the API.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"server_id": schema.StringAttribute{
				MarkdownDescription: "ID (UUID) or slug of the MCP server to connect. Also the " +
					"`terraform import` key. Changing it forces a new connection.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"mcp_server_id": schema.StringAttribute{
				MarkdownDescription: "Resolved UUID of the connected MCP server (equal to `server_id` unless " +
					"a slug was configured).",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "Connection status computed by the platform: `connected`, `pending`, or " +
					"`error`.",
				Computed: true,
			},
			"api_key": schema.StringAttribute{
				MarkdownDescription: "API key, for servers whose directory entry uses the `api_key` " +
					"credential provider. Write-only; changing it forces a new connection.",
				Optional:  true,
				Sensitive: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"bearer_token": schema.StringAttribute{
				MarkdownDescription: "Bearer token, for servers whose directory entry uses the " +
					"`bearer_token` credential provider. Write-only; changing it forces a new connection.",
				Optional:  true,
				Sensitive: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"username": schema.StringAttribute{
				MarkdownDescription: "Username, for servers whose directory entry uses the `basic_auth` " +
					"credential provider. Write-only; changing it forces a new connection.",
				Optional: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"password": schema.StringAttribute{
				MarkdownDescription: "Password, for servers whose directory entry uses the `basic_auth` " +
					"credential provider. Write-only; changing it forces a new connection.",
				Optional:  true,
				Sensitive: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"additional_fields": schema.MapAttribute{
				MarkdownDescription: "Additional credential fields for `generic` (schema-driven) providers, " +
					"as a map of field name to value. Write-only; changing it forces a new connection.",
				ElementType: types.StringType,
				Optional:    true,
				Sensitive:   true,
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.RequiresReplace(),
				},
			},
		},
	}
}

func (r *connectionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *connectionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan connectionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildConnectionCredentials(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid connection configuration", err.Error())
		return
	}

	var initiated connectionInitiateResponse
	connectPath := connectionServerPath(plan.ServerID.ValueString(), "connect") + "?as_service=true"
	if err := doJSON(ctx, r.client, http.MethodPost, connectPath, body, &initiated); err != nil {
		addConnectionAPIError(&resp.Diagnostics, "create the connection", err)
		return
	}
	if initiated.ConnectionID == "" {
		resp.Diagnostics.AddError("Malformed registry API response", "Connect returned no connection id.")
		return
	}
	plan.ID = types.StringValue(initiated.ConnectionID)

	// A returned auth_url means the server's directory entry is an OAuth
	// provider: the platform created a *pending* connection and expects a
	// human to complete the consent in a browser — something an apply cannot
	// do. Track the pending row (so destroy/replace cleans it up) but fail
	// the create loudly.
	if initiated.AuthURL != nil && *initiated.AuthURL != "" {
		plan.McpServerID = types.StringNull()
		plan.Status = types.StringValue("pending")
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError(
			"OAuth MCP servers cannot be connected by Terraform",
			"This MCP server's directory entry uses an OAuth credential provider: connecting it requires an "+
				"interactive browser consent with the upstream provider, which a declarative apply cannot "+
				"perform. The platform created a pending connection, which this resource now tracks — run "+
				"`terraform destroy` (or remove the resource) to clean it up, and complete OAuth connections "+
				"out-of-band (e.g. in the Barndoor app) instead.\n\nOnly api_key, bearer_token, basic_auth, "+
				"and generic credential providers are supported by barndoor_connection.",
		)
		return
	}

	var conn connectionReadResponse
	if err := doJSON(ctx, r.client, http.MethodGet,
		connectionServerPath(plan.ServerID.ValueString(), "connection")+"?as_service=true", nil, &conn); err != nil {
		// The connection exists; track it (with unknowable computed fields
		// nulled) so a subsequent apply reconciles instead of orphaning it.
		resp.Diagnostics.AddError("Failed to read the connection after create", err.Error())
		plan.McpServerID = types.StringNull()
		plan.Status = types.StringNull()
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyConnectionResponse(&conn, &plan))...)
}

func (r *connectionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state connectionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var conn connectionReadResponse
	err := doJSON(ctx, r.client, http.MethodGet,
		connectionServerPath(state.ServerID.ValueString(), "connection")+"?as_service=true", nil, &conn)
	if err != nil {
		if isNotFound(err) {
			// Disconnected out-of-band — or the server itself is gone (both
			// surface as 404); drop the resource so Terraform plans a recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addConnectionAPIError(&resp.Diagnostics, "read the connection", err)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyConnectionResponse(&conn, &state))...)
}

// Update never runs: every configurable attribute forces replacement. The
// framework still requires the method to exist.
func (r *connectionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"Unexpected update of barndoor_connection",
		"Every configurable attribute of barndoor_connection forces replacement, so an in-place update "+
			"should be impossible. This is a bug in the provider.",
	)
}

// Delete removes the service-account connection; the platform cleans up the
// stored credentials. A 404 means it is already gone — success for a destroy.
func (r *connectionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state connectionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete,
		connectionServerPath(state.ServerID.ValueString(), "connection")+"?as_service=true", nil, nil)
	if err != nil && !isNotFound(err) {
		addConnectionAPIError(&resp.Diagnostics, "delete the connection", err)
	}
}

// ImportState imports by the server's id or slug (the connection is keyed by
// its server: one service-account connection per server per organization);
// Read then resolves the connection's own id and status.
func (r *connectionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("server_id"), req, resp)
}

func (r *connectionResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// connectionServerPath builds `/servers/{id}/{leaf}` with the server segment
// path-escaped (it may be a slug).
func connectionServerPath(serverID, leaf string) string {
	return registryAPIPrefix + "/servers/" + url.PathEscape(serverID) + "/" + leaf
}

// addConnectionAPIError turns a registry API error into an actionable
// diagnostic. action is the failed verb phrase, e.g. "create the connection".
func addConnectionAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusBadRequest:
		// The API's message names the missing credential field for the
		// server's provider type (e.g. "API key required for this provider").
		diags.AddError(
			"Connection rejected by the registry API",
			fmt.Sprintf("Failed to %s: %s\n\nSupply the credential attribute matching the server's "+
				"credential provider: `api_key`, `bearer_token`, `username`+`password` (basic auth), or "+
				"`additional_fields` (generic).", action, apiErr.displayBody()),
		)
	case http.StatusNotFound:
		diags.AddError(
			"MCP server not found",
			fmt.Sprintf("Failed to %s: %s\n\nConfirm `server_id` names an existing (non-deleted) MCP server "+
				"in the organization, by UUID or slug.", action, apiErr.displayBody()),
		)
	case http.StatusForbidden:
		diags.AddError(
			"Permission denied by the registry API",
			fmt.Sprintf("Failed to %s: the configured credential is not authorized. Confirm the "+
				"service-account credential carries the organization admin role and is scoped to the "+
				"organization that owns this server.\n\nServer message: %s", action, apiErr.displayBody()),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// --- request/response DTOs ---------------------------------------------------

// connectionCredentialsPayload mirrors the registry CredentialsPayload body.
// Fields are sent only when configured; the server validates the set against
// the directory entry's credential provider.
type connectionCredentialsPayload struct {
	APIKey           *string           `json:"api_key,omitempty"`
	BearerToken      *string           `json:"bearer_token,omitempty"`
	Username         *string           `json:"username,omitempty"`
	Password         *string           `json:"password,omitempty"`
	AdditionalFields map[string]string `json:"additional_fields,omitempty"`
}

// connectionInitiateResponse mirrors the registry ConnectionInitiateResponse.
type connectionInitiateResponse struct {
	ConnectionID string  `json:"connection_id"`
	AuthURL      *string `json:"auth_url"`
	Message      string  `json:"message"`
}

// connectionReadResponse mirrors the registry ConnectionRead.
type connectionReadResponse struct {
	ID          string `json:"id"`
	McpServerID string `json:"mcp_server_id"`
	Status      string `json:"status"`
}

// buildConnectionCredentials converts the planned model to the API body. A
// fully-credential-less body is valid — servers with cascaded pre-populated
// credentials connect without per-connection secrets.
func buildConnectionCredentials(ctx context.Context, plan *connectionResourceModel) (*connectionCredentialsPayload, error) {
	body := &connectionCredentialsPayload{}
	if v, ok := knownString(plan.APIKey); ok {
		body.APIKey = &v
	}
	if v, ok := knownString(plan.BearerToken); ok {
		body.BearerToken = &v
	}
	if v, ok := knownString(plan.Username); ok {
		body.Username = &v
	}
	if v, ok := knownString(plan.Password); ok {
		body.Password = &v
	}
	if !plan.AdditionalFields.IsNull() && !plan.AdditionalFields.IsUnknown() {
		fields := map[string]string{}
		if diags := plan.AdditionalFields.ElementsAs(ctx, &fields, false); diags.HasError() {
			return nil, fmt.Errorf("additional_fields: %v", diags)
		}
		body.AdditionalFields = fields
	}
	return body, nil
}

// applyConnectionResponse maps the server's view onto a state model. The
// write-only credential attributes are carried from prior verbatim because
// the API never returns them.
func applyConnectionResponse(conn *connectionReadResponse, prior *connectionResourceModel) *connectionResourceModel {
	return &connectionResourceModel{
		ID:          types.StringValue(conn.ID),
		ServerID:    prior.ServerID,
		McpServerID: types.StringValue(conn.McpServerID),
		Status:      types.StringValue(conn.Status),

		// Write-only: state follows configuration.
		APIKey:           nullIfUnknownString(prior.APIKey),
		BearerToken:      nullIfUnknownString(prior.BearerToken),
		Username:         nullIfUnknownString(prior.Username),
		Password:         nullIfUnknownString(prior.Password),
		AdditionalFields: nullIfUnknownMap(prior.AdditionalFields),
	}
}

// nullIfUnknownMap settles an unknown map to null so a config-only attribute
// can be written to state (e.g. after import).
func nullIfUnknownMap(v types.Map) types.Map {
	if v.IsUnknown() {
		return types.MapNull(types.StringType)
	}
	return v
}
