// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// identityAPIPrefix is the identity-service public API mount point under the
// platform host root (routed by the edge to identity-service's `/public/v1`
// remount, BCP-3260). Requests are scoped to the credential's organization by
// its Keycloak token claims; there are no org path parameters on this surface.
const identityAPIPrefix = "api/identity/public/v1"

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                = &idpResource{}
	_ resource.ResourceWithConfigure   = &idpResource{}
	_ resource.ResourceWithImportState = &idpResource{}
)

// NewIdpResource returns a new barndoor_idp resource.
func NewIdpResource() resource.Resource {
	return &idpResource{}
}

// idpResource manages the organization's enterprise SSO (OIDC IdP federation)
// connection through the identity-service public REST API
// (`/api/identity/public/v1/idp/connection|config`).
//
// The platform keeps at most one IdP connection per organization (the API's
// POST answers 409 when one exists), so Create treats an existing connection
// as a conflict and directs the practitioner to `terraform import` instead of
// silently adopting — and overwriting — a connection someone configured
// out-of-band. Every attribute updates in place: the platform's PUT mutates
// the existing Keycloak Identity Provider (whose alias derives from the
// organization, not from any configurable attribute), so nothing forces a
// replace.
type idpResource struct {
	client *client.Client
}

// idpResourceModel maps the resource schema to Go types.
type idpResourceModel struct {
	ID               types.String `tfsdk:"id"`
	DisplayName      types.String `tfsdk:"display_name"`
	Issuer           types.String `tfsdk:"issuer"`
	ClientID         types.String `tfsdk:"client_id"`
	ClientSecret     types.String `tfsdk:"client_secret"`
	AuthorizationURL types.String `tfsdk:"authorization_url"`
	TokenURL         types.String `tfsdk:"token_url"`
	UserinfoURL      types.String `tfsdk:"userinfo_url"`
	JwksURL          types.String `tfsdk:"jwks_url"`
	Scopes           types.String `tfsdk:"scopes"`
	Domain           types.String `tfsdk:"domain"`
	AdminGroup       types.String `tfsdk:"admin_group"`
}

func (r *idpResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_idp"
}

func (r *idpResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the organization's **singleton** enterprise SSO connection: the OIDC " +
			"Identity Provider federation (issuer, endpoints, client credentials, scopes) plus the optional " +
			"IdP-group-to-admin-role mapping. An organization holds at most one IdP connection; creating this " +
			"resource fails when one already exists — `terraform import` it instead, so an out-of-band SSO " +
			"setup is not silently overwritten.\n\n" +
			"`client_id` and `client_secret` are **write-only**: the platform never returns them, so Terraform " +
			"tracks the configured values and cannot detect out-of-band credential changes. Every attribute " +
			"updates **in place** — the platform mutates the existing Keycloak Identity Provider, whose alias " +
			"derives from the organization, so changing the issuer or credentials never recreates the " +
			"connection (existing SSO sessions are unaffected; changes apply to new logins).\n\n" +
			"Out of scope (portal-only ceremonies, not on the public API surface): SSO **enforcement** " +
			"(irreversible, member-impacting), break-glass account lifecycle, OIDC discovery auto-fill, and " +
			"SCIM provisioning. The API's connection **test** endpoint (`POST /idp/test`) is not bound either " +
			"— run the preflight from the Barndoor app after applying.\n\n" +
			"`terraform destroy` deletes the Identity Provider and disables SSO for the organization; users " +
			"fall back to their other authentication methods.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Keycloak alias of the Identity Provider, assigned by the platform from " +
					"the organization (`<org-alias>-idp`). The IdP redirect URI shown by the Barndoor app is " +
					"derived from it.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"display_name": schema.StringAttribute{
				MarkdownDescription: "Human-readable name for the IdP, shown on the login screen.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"issuer": schema.StringAttribute{
				MarkdownDescription: "OIDC issuer URL of the external IdP (e.g. `https://company.okta.com`).",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"client_id": schema.StringAttribute{
				MarkdownDescription: "OAuth client ID issued by the external IdP. Write-only: the platform " +
					"only reports whether one is configured, never the value.",
				Required: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"client_secret": schema.StringAttribute{
				MarkdownDescription: "OAuth client secret issued by the external IdP. Write-only: the " +
					"platform stores it in Keycloak and never returns it. Changing it rotates the credential " +
					"in place.",
				Required:  true,
				Sensitive: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"authorization_url": schema.StringAttribute{
				MarkdownDescription: "OIDC authorization endpoint.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"token_url": schema.StringAttribute{
				MarkdownDescription: "OIDC token endpoint.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"userinfo_url": schema.StringAttribute{
				MarkdownDescription: "OIDC userinfo endpoint. Optional; once set, the platform API cannot " +
					"clear it — removing the attribute keeps the current value.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"jwks_url": schema.StringAttribute{
				MarkdownDescription: "OIDC JWKS (signing keys) endpoint.",
				Required:            true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"scopes": schema.StringAttribute{
				MarkdownDescription: "Space-separated OAuth scopes requested from the IdP. The platform " +
					"default is `openid profile email`; when unset, the current server-side value is kept " +
					"and tracked.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"domain": schema.StringAttribute{
				MarkdownDescription: "Email domain for SSO redirect (e.g. `acme.com`): users entering a " +
					"matching email are sent straight to the IdP. **Setting it updates the organization's " +
					"domain** on the platform. When unset, the organization's existing domain is used and " +
					"tracked; once a domain exists the API cannot clear it — removing the attribute keeps " +
					"the current value.",
				Optional: true,
				Computed: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"admin_group": schema.StringAttribute{
				MarkdownDescription: "IdP group name (from the IdP's `groups` claim) whose members are " +
					"granted the Barndoor **admin** role on login. Removing the attribute deletes the " +
					"mapping; members keep only their default role on next login.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
		},
	}
}

func (r *idpResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// Create posts the organization's IdP connection. The API answers 409 when a
// connection already exists — surfaced as an import-instead conflict, not an
// adoption (see the resource doc comment). When `admin_group` is configured,
// the role mapping is written afterwards through its own endpoint; if that
// second call fails, the created connection is still tracked so the next
// apply reconciles instead of orphaning it.
func (r *idpResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan idpResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var conn idpConnectionResponse
	if err := doJSON(ctx, r.client, http.MethodPost, identityAPIPrefix+"/idp/connection",
		buildIdpConnectionWriteRequest(&plan), &conn); err != nil {
		if apiErr, ok := asAPIError(err); ok && apiErr.status == http.StatusConflict {
			resp.Diagnostics.AddError(
				"IdP connection already exists for this organization",
				"The organization already has an Identity Provider connection, and the platform keeps at "+
					"most one. Import it instead of recreating it, so the existing SSO configuration is not "+
					"silently overwritten:\n\n  terraform import <address> <organization-id>\n\nServer "+
					"message: "+apiErr.displayBody(),
			)
			return
		}
		addIdpAPIError(&resp.Diagnostics, "create the IdP connection", err)
		return
	}
	if conn.Alias == nil || *conn.Alias == "" {
		resp.Diagnostics.AddError("Malformed identity API response", "Create returned no IdP alias.")
		return
	}

	mapping := &idpRoleMappingResponse{}
	if v, ok := knownString(plan.AdminGroup); ok {
		if err := doJSON(ctx, r.client, http.MethodPut, identityAPIPrefix+"/idp/config",
			&idpRoleMappingWriteRequest{AdminGroup: &v}, mapping); err != nil {
			// The connection exists; track it (with the mapping unset) so a
			// subsequent apply reconciles instead of orphaning it.
			addIdpAPIError(&resp.Diagnostics, "configure the IdP role mapping after create", err)
			state := applyIdpResponses(&conn, &idpRoleMappingResponse{}, &plan)
			resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
			return
		}
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyIdpResponses(&conn, mapping, &plan))...)
}

func (r *idpResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state idpResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	conn, mapping, gone := r.readIdp(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if gone {
		// Deleted out-of-band (the connection GET stays 200 and signals
		// absence with a null alias); drop the resource so Terraform plans a
		// recreate.
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyIdpResponses(conn, mapping, &state))...)
}

// Update writes the connection through PUT (an in-place mutation of the same
// Keycloak Identity Provider — nothing is recreated) and reconciles the role
// mapping through its own endpoint, each only when its attributes changed.
func (r *idpResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan, state idpResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if idpConnectionChanged(&plan, &state) {
		if err := doJSON(ctx, r.client, http.MethodPut, identityAPIPrefix+"/idp/connection",
			buildIdpConnectionWriteRequest(&plan), nil); err != nil {
			addIdpAPIError(&resp.Diagnostics, "update the IdP connection", err)
			return
		}
	}

	if !plan.AdminGroup.Equal(state.AdminGroup) {
		body := &idpRoleMappingWriteRequest{}
		if v, ok := knownString(plan.AdminGroup); ok {
			body.AdminGroup = &v
		}
		// A null admin_group deletes the mapping server-side.
		if err := doJSON(ctx, r.client, http.MethodPut, identityAPIPrefix+"/idp/config", body, nil); err != nil {
			addIdpAPIError(&resp.Diagnostics, "update the IdP role mapping", err)
			return
		}
	}

	conn, mapping, gone := r.readIdp(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if gone {
		resp.Diagnostics.AddError(
			"IdP connection disappeared during update",
			"The identity API reports no IdP connection for the organization after the update. It was "+
				"likely deleted out-of-band; run `terraform apply` again to recreate it.",
		)
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, applyIdpResponses(conn, mapping, &plan))...)
}

// Delete removes the Identity Provider (and with it the role-mapping
// configuration), disabling SSO for the organization. A 404 means it is
// already gone — success for a destroy.
func (r *idpResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	err := doJSON(ctx, r.client, http.MethodDelete, identityAPIPrefix+"/idp/connection", nil, nil)
	if err != nil && !isNotFound(err) {
		addIdpAPIError(&resp.Diagnostics, "delete the IdP connection", err)
	}
}

// ImportState imports the organization's singleton connection. The API
// resolves the organization from the credential's token claims, so the import
// ID is not looked up — pass the organization ID for consistency; Read
// replaces it with the IdP alias either way. The write-only `client_id` and
// `client_secret` cannot be read back: after import they are null in state,
// and the first plan re-sends the configured values (an in-place update).
func (r *idpResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// readIdp fetches the connection and role-mapping views. gone reports that no
// connection exists (the API signals absence with alias == null on a 200, or
// defensively a 404).
func (r *idpResource) readIdp(ctx context.Context, diags *diag.Diagnostics) (conn *idpConnectionResponse, mapping *idpRoleMappingResponse, gone bool) {
	conn = &idpConnectionResponse{}
	if err := doJSON(ctx, r.client, http.MethodGet, identityAPIPrefix+"/idp/connection", nil, conn); err != nil {
		if isNotFound(err) {
			return nil, nil, true
		}
		addIdpAPIError(diags, "read the IdP connection", err)
		return nil, nil, false
	}
	if conn.Alias == nil || *conn.Alias == "" {
		return nil, nil, true
	}

	mapping = &idpRoleMappingResponse{}
	if err := doJSON(ctx, r.client, http.MethodGet, identityAPIPrefix+"/idp/config", nil, mapping); err != nil {
		addIdpAPIError(diags, "read the IdP role mapping", err)
		return nil, nil, false
	}
	return conn, mapping, false
}

func (r *idpResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

// addIdpAPIError turns an identity API error into an actionable diagnostic.
// action is the failed verb phrase, e.g. "create the IdP connection".
func addIdpAPIError(diags *diag.Diagnostics, action string, err error) {
	apiErr, ok := asAPIError(err)
	if !ok {
		diags.AddError("Failed to "+action, err.Error())
		return
	}

	switch apiErr.status {
	case http.StatusUnauthorized, http.StatusForbidden:
		diags.AddError(
			"Permission denied by the identity API",
			fmt.Sprintf("Failed to %s: the configured credential is not authorized. IdP configuration "+
				"requires the organization **admin** role — confirm the service-account credential carries "+
				"it and is scoped to the organization whose SSO you are managing.\n\nServer message: %s",
				action, apiErr.displayBody()),
		)
	case http.StatusNotFound:
		diags.AddError(
			"IdP connection not found",
			fmt.Sprintf("Failed to %s: the organization has no Identity Provider connection (it may have "+
				"been deleted out-of-band).\n\nServer message: %s", action, apiErr.displayBody()),
		)
	case http.StatusBadRequest:
		diags.AddError(
			"IdP configuration rejected by the identity API",
			fmt.Sprintf("Failed to %s: %s", action, apiErr.displayBody()),
		)
	default:
		diags.AddError("Failed to "+action, apiErr.Error())
	}
}

// --- request/response DTOs ---------------------------------------------------

// idpConnectionWriteRequest mirrors identity-service's IdpConnectionConfig
// body (shared by POST and PUT). The model also accepts `logout_url` and
// `use_discovery`, but the platform never writes the former to the Identity
// Provider (logout deliberately does not propagate to the external IdP) and
// never reads the latter, so the provider does not send them.
type idpConnectionWriteRequest struct {
	DisplayName      string  `json:"display_name"`
	Issuer           string  `json:"issuer"`
	ClientID         *string `json:"client_id,omitempty"`
	ClientSecret     *string `json:"client_secret,omitempty"`
	AuthorizationURL string  `json:"authorization_url"`
	TokenURL         string  `json:"token_url"`
	UserinfoURL      *string `json:"userinfo_url,omitempty"`
	JwksURL          string  `json:"jwks_url"`
	Scopes           *string `json:"scopes,omitempty"`
	Domain           *string `json:"domain,omitempty"`
}

// idpConnectionResponse mirrors identity-service's IdpConnectionResponse.
// client_id and client_secret are never part of it — only the *_configured
// booleans signal their presence.
type idpConnectionResponse struct {
	Configured             bool    `json:"configured"`
	Alias                  *string `json:"alias"`
	ExpectedAlias          *string `json:"expected_alias"`
	DisplayName            *string `json:"display_name"`
	Issuer                 *string `json:"issuer"`
	AuthorizationURL       *string `json:"authorization_url"`
	TokenURL               *string `json:"token_url"`
	UserinfoURL            *string `json:"userinfo_url"`
	JwksURL                *string `json:"jwks_url"`
	Scopes                 *string `json:"scopes"`
	ClientIDConfigured     bool    `json:"client_id_configured"`
	ClientSecretConfigured bool    `json:"client_secret_configured"`
	Domain                 *string `json:"domain"`
}

// idpRoleMappingWriteRequest mirrors identity-service's IdpRoleMappingConfig.
// An explicit null admin_group deletes the mapping, so the field is always
// serialized.
type idpRoleMappingWriteRequest struct {
	AdminGroup *string `json:"admin_group"`
}

// idpRoleMappingResponse mirrors identity-service's IdpRoleMappingResponse.
type idpRoleMappingResponse struct {
	IdpAlias       *string `json:"idp_alias"`
	IdpDisplayName *string `json:"idp_display_name"`
	AdminGroup     *string `json:"admin_group"`
}

// buildIdpConnectionWriteRequest converts the planned model to the API body.
// Optional-but-unclearable fields (userinfo_url, scopes, domain) are sent
// whenever known — after the first apply they are Computed-tracked, so the
// server's current value round-trips instead of being reset (the API's PUT
// skips absent fields, and its POST would fall back to defaults).
func buildIdpConnectionWriteRequest(plan *idpResourceModel) *idpConnectionWriteRequest {
	body := &idpConnectionWriteRequest{
		DisplayName:      plan.DisplayName.ValueString(),
		Issuer:           plan.Issuer.ValueString(),
		AuthorizationURL: plan.AuthorizationURL.ValueString(),
		TokenURL:         plan.TokenURL.ValueString(),
		JwksURL:          plan.JwksURL.ValueString(),
	}
	if v, ok := knownString(plan.ClientID); ok {
		body.ClientID = &v
	}
	if v, ok := knownString(plan.ClientSecret); ok {
		body.ClientSecret = &v
	}
	if v, ok := knownString(plan.UserinfoURL); ok {
		body.UserinfoURL = &v
	}
	if v, ok := knownString(plan.Scopes); ok {
		body.Scopes = &v
	}
	if v, ok := knownString(plan.Domain); ok {
		body.Domain = &v
	}
	return body
}

// idpConnectionChanged reports whether any attribute written through
// PUT /idp/connection differs between plan and state.
func idpConnectionChanged(plan, state *idpResourceModel) bool {
	return !plan.DisplayName.Equal(state.DisplayName) ||
		!plan.Issuer.Equal(state.Issuer) ||
		!plan.ClientID.Equal(state.ClientID) ||
		!plan.ClientSecret.Equal(state.ClientSecret) ||
		!plan.AuthorizationURL.Equal(state.AuthorizationURL) ||
		!plan.TokenURL.Equal(state.TokenURL) ||
		!plan.UserinfoURL.Equal(state.UserinfoURL) ||
		!plan.JwksURL.Equal(state.JwksURL) ||
		!plan.Scopes.Equal(state.Scopes) ||
		!plan.Domain.Equal(state.Domain)
}

// applyIdpResponses maps the server's view onto a state model. The write-only
// client credentials are carried from prior verbatim because the API never
// returns them (only their *_configured booleans).
func applyIdpResponses(conn *idpConnectionResponse, mapping *idpRoleMappingResponse, prior *idpResourceModel) *idpResourceModel {
	return &idpResourceModel{
		ID:               types.StringPointerValue(conn.Alias),
		DisplayName:      types.StringPointerValue(conn.DisplayName),
		Issuer:           types.StringPointerValue(conn.Issuer),
		AuthorizationURL: types.StringPointerValue(conn.AuthorizationURL),
		TokenURL:         types.StringPointerValue(conn.TokenURL),
		UserinfoURL:      types.StringPointerValue(conn.UserinfoURL),
		JwksURL:          types.StringPointerValue(conn.JwksURL),
		Scopes:           types.StringPointerValue(conn.Scopes),
		Domain:           types.StringPointerValue(conn.Domain),
		AdminGroup:       types.StringPointerValue(mapping.AdminGroup),

		// Write-only: state follows configuration.
		ClientID:     nullIfUnknownString(prior.ClientID),
		ClientSecret: nullIfUnknownString(prior.ClientSecret),
	}
}
