// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// registryAPIPrefix is the registry-service public API mount point under the
// platform host root (routed by the edge to registry-service's versioned API).
const registryAPIPrefix = "api/registry/v1"

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &mcpServerResource{}
	_ resource.ResourceWithConfigure      = &mcpServerResource{}
	_ resource.ResourceWithImportState    = &mcpServerResource{}
	_ resource.ResourceWithValidateConfig = &mcpServerResource{}
)

// NewMcpServerResource returns a new barndoor_mcp_server resource.
func NewMcpServerResource() resource.Resource {
	return &mcpServerResource{}
}

// mcpServerResource manages an organization's MCP server instance through the
// registry-service public REST API (`/api/registry/v1/servers`).
type mcpServerResource struct {
	client *client.Client
}

// mcpServerResourceModel maps the resource schema to Go types.
type mcpServerResourceModel struct {
	ID                      types.String         `tfsdk:"id"`
	Name                    types.String         `tfsdk:"name"`
	McpServerDirectoryID    types.String         `tfsdk:"mcp_server_directory_id"`
	Slug                    types.String         `tfsdk:"slug"`
	Status                  types.String         `tfsdk:"status"`
	OauthBaseURLOverride    types.String         `tfsdk:"oauth_base_url_override"`
	UsesManagedCredentials  types.Bool           `tfsdk:"uses_managed_credentials"`
	CredentialMode          types.String         `tfsdk:"credential_mode"`
	ResolvedCredentialMode  types.String         `tfsdk:"resolved_credential_mode"`
	ClientID                types.String         `tfsdk:"client_id"`
	ClientSecret            types.String         `tfsdk:"client_secret"`
	Scopes                  types.List           `tfsdk:"scopes"`
	Meta                    jsontypes.Normalized `tfsdk:"meta"`
	PrepopulatedCredentials jsontypes.Normalized `tfsdk:"prepopulated_credentials"`
	CascadedFields          types.List           `tfsdk:"cascaded_fields"`
	PublishedAt             types.String         `tfsdk:"published_at"`
	AttentionTier           types.String         `tfsdk:"attention_tier"`
	PublishBlockers         types.List           `tfsdk:"publish_blockers"`
}

// The two read-only publish-gate attributes are exposed identically by this
// resource and by the barndoor_mcp_server data source, so their descriptions
// live here and are shared: two copies of this wording would drift, and the
// published docs are generated from it.
const (
	attentionTierDescription = "What the server needs from an administrator next, as one value: " +
		"`pending_publish`, `connection_error`, `pending_credentials`, `pending_connection`, or " +
		"`available` (nothing outstanding). **Read-only**, and computed by the registry rather " +
		"than stored: it is resolved against the organization's `mcp-server-publishing` feature " +
		"flag, so with that flag off a server that is otherwise ready but unpublished reads " +
		"`available` rather than `pending_publish`. Treat it as a reporting signal, not a " +
		"publish precondition — assert on `publish_blockers` for that."

	publishBlockersDescription = "Why a publish would be rejected right now, in the order the " +
		"registry's publish gate evaluates them: `not_operationally_available` (the server has " +
		"no working connection or credentials) and/or `no_active_policy` (no ACTIVE " +
		"`barndoor_policy` targets it). **Read-only.** An empty list means a publish would be " +
		"accepted; **null means undetermined** — the registry could not evaluate the gate " +
		"(the `mcp-server-publishing` feature flag is off for the organization, or " +
		"policy-service did not answer) — so null is not evidence that publishing will succeed."

	storedCredentialModeDescription = "Where the server's OAuth client comes from, as the registry " +
		"stores it: `managed`, `cimd`, `dcr`, `manual`, `static_client` (a per-server client that was " +
		"either entered by hand or registered with the provider, and the registry cannot tell which), " +
		"or `none` (no OAuth client: not configured yet, or the provider does not use OAuth). **Read-only.**"
)

// The credential modes a server create or update request can name. The
// registry stores and returns two more, `static_client` and `none`, which a
// request cannot.
const (
	credentialModeAuto    = "auto"
	credentialModeManaged = "managed"
	credentialModeCIMD    = "cimd"
	credentialModeDCR     = "dcr"
	credentialModeManual  = "manual"
)

var requestedCredentialModes = []string{
	credentialModeAuto, credentialModeManaged, credentialModeCIMD, credentialModeDCR, credentialModeManual,
}

func (r *mcpServerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_server"
}

func (r *mcpServerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an MCP server instance in the organization's registry: the directory " +
			"entry it instantiates, its per-tenant OAuth (or pre-populated) credentials, and scope overrides. " +
			"`terraform destroy` **soft-deletes** the server — the platform tears down its connections and " +
			"stored credentials, and the name/slug are freed for reuse.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Server UUID assigned by the API; also the `terraform import` key.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Display name. Must be unique among the organization's MCP servers " +
					"(case- and whitespace-insensitively).",
				Required: true,
			},
			"mcp_server_directory_id": schema.StringAttribute{
				MarkdownDescription: "ID of the MCP server directory entry this server instantiates. " +
					"Directory entries are catalog data (public or organization-owned) that this resource " +
					"does not manage.",
				Required: true,
			},
			"slug": schema.StringAttribute{
				MarkdownDescription: "URL-safe identifier generated by the API from the name at create time.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "Lifecycle status computed by the platform: `pending` (awaiting " +
					"credentials or an OAuth connection), `active`, or `error`. Creating a server with " +
					"credentials (or `uses_managed_credentials`) activates it immediately; otherwise it stays " +
					"`pending` until a connection is established out-of-band.",
				Computed: true,
			},
			"oauth_base_url_override": schema.StringAttribute{
				MarkdownDescription: "Tenant-specific OAuth base URL override for the upstream provider.",
				Optional:            true,
			},
			"uses_managed_credentials": schema.BoolAttribute{
				MarkdownDescription: "**Deprecated:** use `credential_mode = \"managed\"`. Use " +
					"Barndoor-managed OAuth credentials instead of tenant-supplied ones. The directory entry " +
					"must support managed OAuth (the API rejects it with a 422 otherwise). When both are set " +
					"they must agree.",
				DeprecationMessage: "Use credential_mode = \"managed\" instead.",
				Optional:           true,
			},
			"credential_mode": schema.StringAttribute{
				MarkdownDescription: "Where the server's OAuth client comes from: `managed` (Barndoor's " +
					"managed OAuth client; the directory entry must support it), `cimd` (Barndoor's OAuth " +
					"client ID metadata document; the provider must advertise it and S256 PKCE), `dcr` " +
					"(Barndoor registers a client with the provider through dynamic client registration), " +
					"`manual` (the configured `client_id` and `client_secret`), or `auto` (Barndoor chooses " +
					"from what the provider supports when it creates the server). Omit it for a provider that " +
					"does not use OAuth. **State follows configuration:** the mode the server actually uses is " +
					"`resolved_credential_mode`.\n\n" +
					"Changing it to `managed` or `manual`, or from `cimd` to `dcr`, updates the server in " +
					"place. Changing it to `cimd`, or to `dcr` from any mode other than `cimd`, forces a new " +
					"server, because the registry cannot move an existing server there. Changing it to `auto`, " +
					"or removing it, leaves the server's mode as it is. Any change of mode deletes the " +
					"server's existing user connections.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.OneOf(requestedCredentialModes...),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIf(credentialModeRequiresReplace,
						"Replaces the server when an update cannot move it to the configured credential mode.",
						"Replaces the server when an update cannot move it to the configured credential mode."),
				},
			},
			"resolved_credential_mode": schema.StringAttribute{
				MarkdownDescription: storedCredentialModeDescription + " A change made outside Terraform " +
					"shows here and does not plan a change back to `credential_mode`.",
				Computed: true,
			},
			"client_id": schema.StringAttribute{
				MarkdownDescription: "Tenant OAuth client ID for the upstream provider. **Write-only:** the " +
					"API stores and returns an obfuscated mask, so Terraform tracks the configured value and " +
					"never refreshes it from the server (out-of-band changes are not detected).",
				Optional: true,
			},
			"client_secret": schema.StringAttribute{
				MarkdownDescription: "Tenant OAuth client secret for the upstream provider. Write-only, " +
					"never returned by the API. May be omitted for PKCE public clients.",
				Optional:  true,
				Sensitive: true,
			},
			"scopes": schema.ListAttribute{
				MarkdownDescription: "Server-level OAuth scope override. Omit (or set `[]`… but prefer " +
					"omitting) to inherit the directory entry's default scopes; a non-empty list overrides " +
					"them.",
				ElementType: types.StringType,
				Optional:    true,
			},
			"meta": schema.StringAttribute{
				MarkdownDescription: "Server metadata as a JSON object (`jsonencode({...})`), e.g. the " +
					"`access_context` values for directory-templated properties. **State follows " +
					"configuration:** the platform merges computed defaults into stored metadata, and those " +
					"server-computed keys are deliberately not tracked here.",
				Optional:   true,
				CustomType: jsontypes.NormalizedType{},
			},
			"prepopulated_credentials": schema.StringAttribute{
				MarkdownDescription: "Pre-populated credentials for non-OAuth providers as a JSON object " +
					"(e.g. `jsonencode({api_key = \"...\"})`). Write-only and **create-only**: the API has no " +
					"update path for it, so changing it forces a new server.",
				Optional:   true,
				Sensitive:  true,
				CustomType: jsontypes.NormalizedType{},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cascaded_fields": schema.ListAttribute{
				MarkdownDescription: "Names of pre-populated credential fields that cascade from the server " +
					"to user connections. Create-only: changing it forces a new server.",
				ElementType: types.StringType,
				Optional:    true,
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
				},
			},
			"published_at": schema.StringAttribute{
				MarkdownDescription: "RFC 3339 timestamp of when the server was published (made " +
					"discoverable to end users); null while unpublished. **Read-only** — publish with the " +
					"`barndoor_mcp_server_publication` resource. Publishing is one-way: there is no " +
					"unpublish.",
				Computed: true,
			},
			"attention_tier": schema.StringAttribute{
				MarkdownDescription: attentionTierDescription,
				Computed:            true,
			},
			"publish_blockers": schema.ListAttribute{
				MarkdownDescription: publishBlockersDescription,
				ElementType:         types.StringType,
				Computed:            true,
			},
		},
	}
}

func (r *mcpServerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ValidateConfig rejects JSON attributes that are valid JSON but not objects —
// the API requires `meta` and `prepopulated_credentials` to be JSON objects,
// so catch a bare array/string/number at plan time.
func (r *mcpServerResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg mcpServerResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	validateJSONObject(cfg.Meta, path.Root("meta"), &resp.Diagnostics)
	validateJSONObject(cfg.PrepopulatedCredentials, path.Root("prepopulated_credentials"), &resp.Diagnostics)

	if !cfg.CascadedFields.IsNull() && !cfg.CascadedFields.IsUnknown() &&
		len(cfg.CascadedFields.Elements()) > 0 &&
		cfg.PrepopulatedCredentials.IsNull() {
		resp.Diagnostics.AddAttributeError(
			path.Root("cascaded_fields"),
			"cascaded_fields requires prepopulated_credentials",
			"`cascaded_fields` names fields of `prepopulated_credentials` to cascade to user connections, "+
				"but no `prepopulated_credentials` are configured.",
		)
	}

	validateCredentialMode(&cfg, &resp.Diagnostics)
}

// validateCredentialMode rejects the credential_mode combinations the registry
// refuses, and the ones it accepts but resolves to a mode other than the one
// credential_mode names: a client_id outranks every mode but `manual`.
func validateCredentialMode(cfg *mcpServerResourceModel, diags *diag.Diagnostics) {
	mode, ok := knownString(cfg.CredentialMode)
	if !ok {
		return
	}

	if !cfg.UsesManagedCredentials.IsNull() && !cfg.UsesManagedCredentials.IsUnknown() &&
		cfg.UsesManagedCredentials.ValueBool() != (mode == credentialModeManaged) {
		diags.AddAttributeError(
			path.Root("uses_managed_credentials"),
			"uses_managed_credentials conflicts with credential_mode",
			fmt.Sprintf("`uses_managed_credentials = %t` contradicts `credential_mode = %q`. Remove "+
				"`uses_managed_credentials`; `credential_mode` replaces it.",
				cfg.UsesManagedCredentials.ValueBool(), mode),
		)
	}

	if cfg.ClientID.IsUnknown() {
		return
	}

	switch {
	case mode == credentialModeManual && cfg.ClientID.IsNull():
		diags.AddAttributeError(
			path.Root("client_id"),
			"credential_mode \"manual\" requires client_id",
			"A `manual` server uses the configured `client_id` (and `client_secret`, when the provider "+
				"needs one); set `client_id`.",
		)
	case mode != credentialModeManual && !cfg.ClientID.IsNull():
		diags.AddAttributeError(
			path.Root("client_id"),
			"client_id requires credential_mode \"manual\"",
			fmt.Sprintf("The registry uses a configured `client_id` in place of `credential_mode = %q`. "+
				"Set `credential_mode = \"manual\"` to use the client ID, or remove `client_id`.", mode),
		)
	}
}

// credentialModeRequiresReplace forces a new server when an update cannot move
// the existing one to the configured mode: the registry never moves a server
// into `cimd`, and moves one into `dcr` only from `cimd`. Naming the mode the
// server already uses is a no-op update, so it never replaces.
func credentialModeRequiresReplace(ctx context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	planned, ok := knownString(req.PlanValue)
	if !ok {
		return
	}

	var resolved types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("resolved_credential_mode"), &resolved)...)
	current := resolved.ValueString()

	switch planned {
	case credentialModeCIMD:
		resp.RequiresReplace = current != credentialModeCIMD
	case credentialModeDCR:
		resp.RequiresReplace = current != credentialModeCIMD && current != credentialModeDCR
	}
}

// validateJSONObject adds a diagnostic when v holds known JSON that is not an
// object.
func validateJSONObject(v jsontypes.Normalized, p path.Path, diags *diag.Diagnostics) {
	if v.IsNull() || v.IsUnknown() {
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(v.ValueString()), &obj); err != nil {
		diags.AddAttributeError(p, "Value must be a JSON object",
			fmt.Sprintf("Expected a JSON object (e.g. `jsonencode({...})`): %s.", err))
	}
}

func (r *mcpServerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan mcpServerResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildMcpServerWriteRequest(ctx, &plan, true)
	if err != nil {
		resp.Diagnostics.AddError("Invalid MCP server configuration", err.Error())
		return
	}

	var created mcpServerCreateResponse
	if err := doJSON(ctx, r.client, http.MethodPost, registryAPIPrefix+"/servers", body, &created); err != nil {
		addMcpServerAPIError(&resp.Diagnostics, "create the MCP server", err)
		return
	}
	if created.ID == "" {
		resp.Diagnostics.AddError("Malformed registry API response", "Create returned no server id.")
		return
	}
	plan.ID = types.StringValue(created.ID)

	// The create endpoint returns only the id — fetch the full object for an
	// authoritative state (slug, status, normalized fields).
	var server mcpServerResponse
	if err := doJSON(ctx, r.client, http.MethodGet, registryAPIPrefix+"/servers/"+created.ID, nil, &server); err != nil {
		// The server exists; track it (with unknowable computed fields nulled)
		// so a subsequent apply reconciles instead of orphaning it.
		resp.Diagnostics.AddError("Failed to read the MCP server after create", err.Error())
		plan.Slug = types.StringNull()
		plan.Status = types.StringNull()
		plan.ResolvedCredentialMode = types.StringNull()
		plan.PublishedAt = types.StringNull()
		plan.AttentionTier = types.StringNull()
		plan.PublishBlockers = types.ListNull(types.StringType)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	state, err := applyMcpServerResponse(ctx, &server, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the registry API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *mcpServerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state mcpServerResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var server mcpServerResponse
	err := doJSON(ctx, r.client, http.MethodGet, registryAPIPrefix+"/servers/"+state.ID.ValueString(), nil, &server)
	if err != nil {
		if isNotFound(err) {
			// Deleted out-of-band (the API 404s soft-deleted servers); drop it
			// so Terraform plans a recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		addMcpServerAPIError(&resp.Diagnostics, "read the MCP server", err)
		return
	}

	newState, err := applyMcpServerResponse(ctx, &server, &state)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the registry API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *mcpServerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan, prior mcpServerResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildMcpServerWriteRequest(ctx, &plan, false)
	if err != nil {
		resp.Diagnostics.AddError("Invalid MCP server configuration", err.Error())
		return
	}
	applyCredentialModeUpdate(body, &plan, &prior)

	var server mcpServerResponse
	if err := doJSON(ctx, r.client, http.MethodPut, registryAPIPrefix+"/servers/"+plan.ID.ValueString(), body, &server); err != nil {
		addMcpServerAPIError(&resp.Diagnostics, "update the MCP server", err)
		return
	}

	// The PUT response is the stored record only: it omits the per-read
	// attention_tier / publish_blockers, which would land in state as null
	// until the next refresh. Re-read for the full object, like Create.
	// On a failed re-read the update still happened, so keep the PUT's view.
	var fresh mcpServerResponse
	if err := doJSON(ctx, r.client, http.MethodGet, registryAPIPrefix+"/servers/"+plan.ID.ValueString(), nil, &fresh); err != nil {
		resp.Diagnostics.AddError("Failed to read the MCP server after update", err.Error())
	} else {
		server = fresh
	}

	newState, err := applyMcpServerResponse(ctx, &server, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the registry API response", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Delete soft-deletes the server; the platform cascades connection teardown
// and credential cleanup. The API is idempotent (a re-delete returns 204) and
// a 404 means it is already gone — success for a destroy either way.
func (r *mcpServerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state mcpServerResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete, registryAPIPrefix+"/servers/"+state.ID.ValueString(), nil, nil)
	if err != nil && !isNotFound(err) {
		addMcpServerAPIError(&resp.Diagnostics, "delete the MCP server", err)
	}
}

func (r *mcpServerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *mcpServerResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// addMcpServerAPIError turns a registry API error into an actionable
// diagnostic. action is the failed verb phrase, e.g. "create the MCP server".
func addMcpServerAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusConflict:
		diags.AddError(
			"MCP server name or slug already in use",
			fmt.Sprintf("Failed to %s: %s\n\nServer names must be unique among the organization's "+
				"non-deleted MCP servers; choose a different `name` or remove the conflicting server first.",
				action, apiErr.displayBody()),
		)
	case http.StatusUnprocessableEntity:
		// The API's validation message (bad directory id, unsupported managed
		// OAuth, invalid access_context, …) is the most specific thing we can
		// show; surface it verbatim.
		diags.AddError("MCP server rejected by the registry API", apiErr.displayBody())
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

// mcpServerWriteRequest mirrors the registry ServerCreateUpdateRequest body.
//
// oauth_base_url_override, uses_managed_credentials, and scopes are always
// sent (explicit null when unset): the update endpoint only touches fields
// present in the payload, so omitting a removed field would leave the old
// value behind and the configuration could never converge. client_id /
// client_secret / meta / prepopulated_credentials / cascaded_fields are sent
// only when configured — an explicit null would be meaningless (credentials)
// or a no-op (meta) server-side. credential_mode is sent on create when
// configured, and on update only as applyCredentialModeUpdate decides.
type mcpServerWriteRequest struct {
	Name                    string          `json:"name"`
	McpServerDirectoryID    string          `json:"mcp_server_directory_id"`
	OauthBaseURLOverride    *string         `json:"oauth_base_url_override"`
	UsesManagedCredentials  *bool           `json:"uses_managed_credentials"`
	CredentialMode          *string         `json:"credential_mode,omitempty"`
	Scopes                  []string        `json:"scopes"`
	ClientID                *string         `json:"client_id,omitempty"`
	ClientSecret            *string         `json:"client_secret,omitempty"`
	Meta                    json.RawMessage `json:"meta,omitempty"`
	PrepopulatedCredentials json.RawMessage `json:"prepopulated_credentials,omitempty"`
	CascadedFields          []string        `json:"cascaded_fields,omitempty"`
}

// mcpServerCreateResponse mirrors the create endpoint's ServerCreateResponse.
type mcpServerCreateResponse struct {
	ID string `json:"id"`
}

// mcpServerResponse mirrors the subset of the registry ServerResponse this
// resource manages. Credential fields are deliberately absent: the API returns
// obfuscated masks, which must never overwrite the configured values in state.
type mcpServerResponse struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Slug                   string   `json:"slug"`
	Status                 string   `json:"status"`
	McpServerDirectoryID   string   `json:"mcp_server_directory_id"`
	OauthBaseURLOverride   *string  `json:"oauth_base_url_override"`
	UsesManagedCredentials *bool    `json:"uses_managed_credentials"`
	CredentialMode         *string  `json:"credential_mode"`
	Scopes                 []string `json:"scopes"`
	PublishedAt            *string  `json:"published_at"`
	AttentionTier          *string  `json:"attention_tier"`
	// PublishBlockers is a POINTER to a slice because the registry's null and
	// [] carry different meanings that must both survive to state: null is
	// "undetermined" (the publishing feature flag is off, or policy-service did
	// not answer) while [] is the positive "a publish would be accepted". The
	// pointer makes that distinction explicit and un-loseable. A bare []string
	// can in principle carry it too — encoding/json leaves the field nil for
	// null/absent and allocates an empty slice for `[]` — but nil-vs-empty on a
	// slice is invisible to len(), survives no append or normalization step,
	// and is preserved only by convention; a nil pointer cannot be mistaken for
	// an empty one. The wire contract itself is pinned by
	// TestMcpServerResponse_PublishGateFieldsUnmarshal.
	PublishBlockers *[]string `json:"publish_blockers"`
}

// buildMcpServerWriteRequest converts the planned model to the API body.
// create toggles the create-only fields (prepopulated_credentials,
// cascaded_fields) — the update endpoint ignores them, so they are simply not
// sent there.
func buildMcpServerWriteRequest(ctx context.Context, plan *mcpServerResourceModel, create bool) (*mcpServerWriteRequest, error) {
	body := &mcpServerWriteRequest{
		Name:                 plan.Name.ValueString(),
		McpServerDirectoryID: plan.McpServerDirectoryID.ValueString(),
	}
	if v, ok := knownString(plan.OauthBaseURLOverride); ok {
		body.OauthBaseURLOverride = &v
	}
	if !plan.UsesManagedCredentials.IsNull() && !plan.UsesManagedCredentials.IsUnknown() {
		v := plan.UsesManagedCredentials.ValueBool()
		body.UsesManagedCredentials = &v
	}

	mode, hasMode := knownString(plan.CredentialMode)
	if create && hasMode {
		body.CredentialMode = &mode
	}
	// An update writes uses_managed_credentials from the payload whether or
	// not it is sent, so a managed server must carry `true` on every update,
	// including the ones that do not send credential_mode.
	if body.UsesManagedCredentials == nil && mode == credentialModeManaged {
		v := true
		body.UsesManagedCredentials = &v
	}

	if v, ok := knownString(plan.ClientID); ok {
		body.ClientID = &v
	}
	if v, ok := knownString(plan.ClientSecret); ok {
		body.ClientSecret = &v
	}

	var err error
	if body.Scopes, err = stringsFromList(ctx, plan.Scopes); err != nil {
		return nil, fmt.Errorf("scopes: %w", err)
	}

	if v, ok := knownNormalized(plan.Meta); ok {
		body.Meta = json.RawMessage(v)
	}
	if create {
		if v, ok := knownNormalized(plan.PrepopulatedCredentials); ok {
			body.PrepopulatedCredentials = json.RawMessage(v)
		}
		if body.CascadedFields, err = stringsFromList(ctx, plan.CascadedFields); err != nil {
			return nil, fmt.Errorf("cascaded_fields: %w", err)
		}
	}
	return body, nil
}

// applyCredentialModeUpdate sets the update body's credential_mode from the
// planned and prior configuration. It is sent only when the configured value
// changed to a mode an update can name: re-sending an unchanged mode would
// turn a mode changed outside Terraform back, and an update refuses `auto`.
//
// The update writes uses_managed_credentials from every payload, so when the
// configuration has never set the flag and the server is managed, the flag is
// sent as `true`: an unrelated update, or removing credential_mode, must not
// move the server off its managed client.
func applyCredentialModeUpdate(body *mcpServerWriteRequest, plan, prior *mcpServerResourceModel) {
	mode, ok := knownString(plan.CredentialMode)
	if ok && mode != credentialModeAuto && !plan.CredentialMode.Equal(prior.CredentialMode) {
		body.CredentialMode = &mode
		return
	}

	if body.UsesManagedCredentials == nil && plan.UsesManagedCredentials.IsNull() &&
		prior.UsesManagedCredentials.IsNull() && prior.ResolvedCredentialMode.ValueString() == credentialModeManaged {
		v := true
		body.UsesManagedCredentials = &v
	}
}

// applyMcpServerResponse maps the server's view onto a state model. prior is
// the plan (Create/Update) or previous state (Read); write-only attributes
// (client_id, client_secret, meta, prepopulated_credentials, cascaded_fields)
// are carried from it verbatim because the API never returns their real
// values.
func applyMcpServerResponse(ctx context.Context, server *mcpServerResponse, prior *mcpServerResourceModel) (mcpServerResourceModel, error) {
	m := mcpServerResourceModel{
		ID:                   types.StringValue(server.ID),
		Name:                 types.StringValue(server.Name),
		McpServerDirectoryID: types.StringValue(server.McpServerDirectoryID),
		Slug:                 types.StringValue(server.Slug),
		Status:               types.StringValue(server.Status),
		OauthBaseURLOverride: optionalStringFromPtr(server.OauthBaseURLOverride, prior.OauthBaseURLOverride),
		PublishedAt:          types.StringPointerValue(server.PublishedAt),
		AttentionTier:        types.StringPointerValue(server.AttentionTier),

		// `auto` resolves to a different stored mode, and a mode changed outside
		// Terraform is reported rather than planned back, so the configured
		// value is carried and the stored one goes to resolved_credential_mode.
		CredentialMode:         nullIfUnknownString(prior.CredentialMode),
		ResolvedCredentialMode: types.StringPointerValue(server.CredentialMode),

		// Write-only: state follows configuration.
		ClientID:                nullIfUnknownString(prior.ClientID),
		ClientSecret:            nullIfUnknownString(prior.ClientSecret),
		Meta:                    nullIfUnknownNormalized(prior.Meta),
		PrepopulatedCredentials: nullIfUnknownNormalized(prior.PrepopulatedCredentials),
	}

	m.CascadedFields = prior.CascadedFields
	if m.CascadedFields.IsUnknown() {
		m.CascadedFields = types.ListNull(types.StringType)
	}

	// An unset flag follows configuration: the provider derives it from
	// credential_mode, and a server made managed outside Terraform is reported
	// in resolved_credential_mode rather than planned back.
	if prior.UsesManagedCredentials.IsNull() || server.UsesManagedCredentials == nil {
		m.UsesManagedCredentials = types.BoolNull()
	} else {
		m.UsesManagedCredentials = types.BoolValue(*server.UsesManagedCredentials)
	}

	var err error
	if m.Scopes, err = listFromStrings(ctx, server.Scopes, prior.Scopes); err != nil {
		return mcpServerResourceModel{}, fmt.Errorf("scopes: %w", err)
	}
	if m.PublishBlockers, err = nullableListFromStrings(ctx, server.PublishBlockers); err != nil {
		return mcpServerResourceModel{}, fmt.Errorf("publish_blockers: %w", err)
	}
	return m, nil
}

// nullableListFromStrings maps a nullable wire string list to a computed-only
// state list, preserving the distinction the sender drew: a null pointer (JSON
// null, or the key absent) becomes a null list and a non-nil pointer becomes a
// concrete list — `[]` included. Unlike listFromStrings it never settles an
// empty list to null, because for these attributes empty and null are
// different answers, not the same absence.
func nullableListFromStrings(ctx context.Context, vals *[]string) (types.List, error) {
	if vals == nil {
		return types.ListNull(types.StringType), nil
	}
	return computedListFromStrings(ctx, *vals)
}

// listFromStrings maps a wire string list to state, settling an empty
// collection to null when the prior value was null/unknown (the server cannot
// distinguish "cleared" from "never set", and echoing [] where the config said
// nothing would be perpetual drift).
func listFromStrings(ctx context.Context, vals []string, prior types.List) (types.List, error) {
	if len(vals) == 0 && (prior.IsNull() || prior.IsUnknown()) {
		return types.ListNull(types.StringType), nil
	}
	if vals == nil {
		vals = []string{}
	}
	list, diags := types.ListValueFrom(ctx, types.StringType, vals)
	if diags.HasError() {
		return types.ListNull(types.StringType), fmt.Errorf("%v", diags)
	}
	return list, nil
}

// nullIfUnknownString settles an unknown value to null so a config-only
// attribute can be written to state (e.g. after import, where no prior config
// value exists).
func nullIfUnknownString(v types.String) types.String {
	if v.IsUnknown() {
		return types.StringNull()
	}
	return v
}

// nullIfUnknownNormalized is nullIfUnknownString for normalized-JSON values.
func nullIfUnknownNormalized(v jsontypes.Normalized) jsontypes.Normalized {
	if v.IsUnknown() {
		return jsontypes.NewNormalizedNull()
	}
	return v
}

// knownNormalized returns the raw JSON text and true when v is a known,
// non-null normalized-JSON value.
func knownNormalized(v jsontypes.Normalized) (string, bool) {
	if v.IsNull() || v.IsUnknown() {
		return "", false
	}
	return v.ValueString(), true
}
