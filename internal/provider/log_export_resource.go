// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// defaultExportType is the export type managed when the practitioner does not
// set export_type. It matches the only customer-facing export the platform
// currently provisions per organization.
const defaultExportType = "datadog-json"

const (
	authMethodAccessKeys = "access_keys"
	authMethodIAMRole    = "iam_role"
)

// Ensure the resource satisfies the framework interfaces it relies on.
var (
	_ resource.Resource                   = &logExportResource{}
	_ resource.ResourceWithConfigure      = &logExportResource{}
	_ resource.ResourceWithImportState    = &logExportResource{}
	_ resource.ResourceWithValidateConfig = &logExportResource{}
)

// NewLogExportResource returns a new barndoor_log_export resource.
func NewLogExportResource() resource.Resource {
	return &logExportResource{}
}

// logExportResource manages an organization's audit-log export configuration
// (destination + settings + enabled state) through the system-management
// service public API.
type logExportResource struct {
	client *client.Client
}

// logExportResourceModel maps the resource schema to Go types.
type logExportResourceModel struct {
	OrganizationID types.String      `tfsdk:"organization_id"`
	ExportType     types.String      `tfsdk:"export_type"`
	Enabled        types.Bool        `tfsdk:"enabled"`
	Destination    *destinationModel `tfsdk:"destination"`
	Settings       *settingsModel    `tfsdk:"settings"`
}

type destinationModel struct {
	Endpoint        types.String `tfsdk:"endpoint"`
	Region          types.String `tfsdk:"region"`
	Bucket          types.String `tfsdk:"bucket"`
	PathPrefix      types.String `tfsdk:"path_prefix"`
	UseSSL          types.Bool   `tfsdk:"use_ssl"`
	UsePathStyle    types.Bool   `tfsdk:"use_path_style"`
	AuthMethod      types.String `tfsdk:"auth_method"`
	IAMRoleArn      types.String `tfsdk:"iam_role_arn"`
	AccessKeyID     types.String `tfsdk:"access_key_id"`
	SecretAccessKey types.String `tfsdk:"secret_access_key"`
	ExternalID      types.String `tfsdk:"external_id"`
	HasCredentials  types.Bool   `tfsdk:"has_credentials"`
}

type settingsModel struct {
	BatchSize            types.Int64 `tfsdk:"batch_size"`
	FlushIntervalSeconds types.Int64 `tfsdk:"flush_interval_seconds"`
	MaxRetries           types.Int64 `tfsdk:"max_retries"`
	IncludedEventTypes   types.List  `tfsdk:"included_event_types"`
}

func (r *logExportResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_log_export"
}

func (r *logExportResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Barndoor organization's audit-log export: the customer-owned " +
			"S3-compatible destination, delivery settings, and whether streaming is enabled. The export " +
			"row itself is provisioned per organization by the platform; this resource configures it.",
		Attributes: map[string]schema.Attribute{
			"organization_id": schema.StringAttribute{
				MarkdownDescription: "Organization ID (Keycloak organization UUID) that owns the export. " +
					"Defaults to the provider's `organization_id`. Changing this forces a new resource.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"export_type": schema.StringAttribute{
				MarkdownDescription: "Export type to configure. Defaults to `" + defaultExportType + "`. " +
					"Changing this forces a new resource.",
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(defaultExportType),
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether audit-log streaming is enabled. When `true`, the destination " +
					"must be fully configured and reachable — the API runs a connectivity probe before " +
					"streaming starts. Defaults to `false`.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
			},
			"destination": schema.SingleNestedAttribute{
				MarkdownDescription: "Customer-owned S3-compatible bucket that exported audit logs are " +
					"written to. Identifier values must not have surrounding whitespace; omit optional " +
					"fields rather than setting them to an empty string.",
				Required:   true,
				Attributes: destinationSchemaAttributes(),
			},
			"settings": schema.SingleNestedAttribute{
				MarkdownDescription: "Delivery tuning for the export. Omit to leave the platform defaults " +
					"in place; any attribute left unset is filled from the server and tracked as computed.",
				Optional:   true,
				Attributes: settingsSchemaAttributes(),
			},
		},
	}
}

func destinationSchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"endpoint": schema.StringAttribute{
			MarkdownDescription: "S3 endpoint URL, e.g. `https://s3.us-east-1.amazonaws.com` or an " +
				"S3-compatible endpoint.",
			Required: true,
		},
		"bucket": schema.StringAttribute{
			MarkdownDescription: "Destination bucket name.",
			Required:            true,
		},
		"region": schema.StringAttribute{
			MarkdownDescription: "Bucket region (e.g. `us-east-1`).",
			Optional:            true,
		},
		"path_prefix": schema.StringAttribute{
			MarkdownDescription: "Optional key prefix that exported objects are written under.",
			Optional:            true,
		},
		"use_ssl": schema.BoolAttribute{
			MarkdownDescription: "Whether to connect to the endpoint over TLS. Defaults to `true`.",
			Optional:            true,
			Computed:            true,
			Default:             booldefault.StaticBool(true),
		},
		"use_path_style": schema.BoolAttribute{
			MarkdownDescription: "Whether to use path-style bucket addressing (required by some " +
				"S3-compatible stores). Defaults to `false`.",
			Optional: true,
			Computed: true,
			Default:  booldefault.StaticBool(false),
		},
		"auth_method": schema.StringAttribute{
			MarkdownDescription: "How Barndoor authenticates to the bucket: `access_keys` (default) or " +
				"`iam_role`. `iam_role` requires the feature to be enabled for the organization.",
			Optional: true,
			Computed: true,
			Default:  stringdefault.StaticString(authMethodAccessKeys),
		},
		"iam_role_arn": schema.StringAttribute{
			MarkdownDescription: "ARN of the IAM role Barndoor assumes to write to the bucket. Required " +
				"when `auth_method` is `iam_role`; must be omitted otherwise.",
			Optional: true,
		},
		"access_key_id": schema.StringAttribute{
			MarkdownDescription: "Access key ID for the bucket. Required when `auth_method` is " +
				"`access_keys`; must be omitted otherwise. Not returned by the API, so it is tracked " +
				"only from configuration.",
			Optional: true,
		},
		"secret_access_key": schema.StringAttribute{
			MarkdownDescription: "Secret access key for the bucket. Required when `auth_method` is " +
				"`access_keys`; must be omitted otherwise. Never returned by the API.",
			Optional:  true,
			Sensitive: true,
		},
		"external_id": schema.StringAttribute{
			MarkdownDescription: "Computed `sts:ExternalId` minted for the destination's IAM trust policy. " +
				"Populated only when `auth_method` is `iam_role`.",
			Computed: true,
		},
		"has_credentials": schema.BoolAttribute{
			MarkdownDescription: "Computed flag indicating whether the API has stored access-key " +
				"credentials for this destination.",
			Computed: true,
		},
	}
}

func settingsSchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"batch_size": schema.Int64Attribute{
			MarkdownDescription: "Number of events to batch before flushing (must be at least 1). " +
				"Computed from the server when unset.",
			Optional: true,
			Computed: true,
			PlanModifiers: []planmodifier.Int64{
				int64planmodifier.UseStateForUnknown(),
			},
		},
		"flush_interval_seconds": schema.Int64Attribute{
			MarkdownDescription: "Maximum time, in seconds, to wait before flushing a batch (must be at " +
				"least 1). Computed from the server when unset.",
			Optional: true,
			Computed: true,
			PlanModifiers: []planmodifier.Int64{
				int64planmodifier.UseStateForUnknown(),
			},
		},
		"max_retries": schema.Int64Attribute{
			MarkdownDescription: "Maximum number of retries for a failed delivery (must be at least 1). " +
				"Computed from the server when unset.",
			Optional: true,
			Computed: true,
			PlanModifiers: []planmodifier.Int64{
				int64planmodifier.UseStateForUnknown(),
			},
		},
		"included_event_types": schema.ListAttribute{
			MarkdownDescription: "Event types to deliver. An empty or unset list delivers all event types.",
			ElementType:         types.StringType,
			Optional:            true,
			Computed:            true,
			PlanModifiers: []planmodifier.List{
				listplanmodifier.UseStateForUnknown(),
			},
		},
	}
}

func (r *logExportResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ValidateConfig enforces the cross-field rules the API would otherwise reject
// at apply time, so practitioners get errors during plan.
func (r *logExportResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg logExportResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if cfg.Destination != nil {
		validateDestinationConfig(cfg.Destination, &resp.Diagnostics)
		validateDestinationStrings(cfg.Destination, &resp.Diagnostics)
	}
	if cfg.Settings != nil {
		validateSettingsConfig(cfg.Settings, &resp.Diagnostics)
	}

	// enabled streaming requires a destination; the API rejects start otherwise.
	if !cfg.Enabled.IsNull() && !cfg.Enabled.IsUnknown() && cfg.Enabled.ValueBool() && cfg.Destination == nil {
		resp.Diagnostics.AddAttributeError(
			path.Root("enabled"),
			"Destination required to enable streaming",
			"`enabled` is true but no `destination` is configured; the export cannot start without one.",
		)
	}
}

func validateDestinationConfig(d *destinationModel, diags *diag.Diagnostics) {
	base := path.Root("destination")

	authMethod := authMethodAccessKeys
	if !d.AuthMethod.IsNull() && !d.AuthMethod.IsUnknown() {
		authMethod = d.AuthMethod.ValueString()
	}

	switch authMethod {
	case authMethodAccessKeys:
		if isNullKnown(d.AccessKeyID) {
			diags.AddAttributeError(base.AtName("access_key_id"),
				"Missing access_key_id",
				"`access_key_id` is required when `auth_method` is `access_keys`.")
		}
		if isNullKnown(d.SecretAccessKey) {
			diags.AddAttributeError(base.AtName("secret_access_key"),
				"Missing secret_access_key",
				"`secret_access_key` is required when `auth_method` is `access_keys`.")
		}
		if isSetKnown(d.IAMRoleArn) {
			diags.AddAttributeError(base.AtName("iam_role_arn"),
				"Unexpected iam_role_arn",
				"`iam_role_arn` must not be set when `auth_method` is `access_keys`.")
		}
	case authMethodIAMRole:
		if isNullKnown(d.IAMRoleArn) {
			diags.AddAttributeError(base.AtName("iam_role_arn"),
				"Missing iam_role_arn",
				"`iam_role_arn` is required when `auth_method` is `iam_role`.")
		}
		if isSetKnown(d.AccessKeyID) {
			diags.AddAttributeError(base.AtName("access_key_id"),
				"Unexpected access_key_id",
				"`access_key_id` must not be set when `auth_method` is `iam_role`.")
		}
		if isSetKnown(d.SecretAccessKey) {
			diags.AddAttributeError(base.AtName("secret_access_key"),
				"Unexpected secret_access_key",
				"`secret_access_key` must not be set when `auth_method` is `iam_role`.")
		}
	default:
		diags.AddAttributeError(base.AtName("auth_method"),
			"Invalid auth_method",
			fmt.Sprintf("`auth_method` must be %q or %q, got %q.", authMethodAccessKeys, authMethodIAMRole, authMethod))
	}
}

// validateDestinationStrings rejects empty and whitespace-padded identifier
// values. The API trims these fields server-side (see the SMS destination
// handler), so a padded value would read back trimmed and surface as a
// "provider produced an inconsistent result" error; an explicit empty string
// would read back as null for the same reason. Rejecting both at plan time
// turns those into a clear, actionable message.
func validateDestinationStrings(d *destinationModel, diags *diag.Diagnostics) {
	base := path.Root("destination")
	fields := []struct {
		name string
		v    types.String
	}{
		{"endpoint", d.Endpoint},
		{"bucket", d.Bucket},
		{"region", d.Region},
		{"path_prefix", d.PathPrefix},
		{"iam_role_arn", d.IAMRoleArn},
		{"access_key_id", d.AccessKeyID},
		{"secret_access_key", d.SecretAccessKey},
	}
	for _, f := range fields {
		if f.v.IsNull() || f.v.IsUnknown() {
			continue
		}
		s := f.v.ValueString()
		if s != strings.TrimSpace(s) {
			// Note: the value itself is never echoed, keeping secrets out of diagnostics.
			diags.AddAttributeError(base.AtName(f.name),
				"Surrounding whitespace not allowed",
				fmt.Sprintf("`%s` has leading or trailing whitespace. The API trims it, which would cause persistent drift; remove the surrounding whitespace.", f.name))
			continue
		}
		if s == "" {
			diags.AddAttributeError(base.AtName(f.name),
				"Value must not be empty",
				fmt.Sprintf("`%s` is set to an empty string; omit the attribute instead.", f.name))
		}
	}
}

func validateSettingsConfig(s *settingsModel, diags *diag.Diagnostics) {
	base := path.Root("settings")
	for name, v := range map[string]types.Int64{
		"batch_size":             s.BatchSize,
		"flush_interval_seconds": s.FlushIntervalSeconds,
		"max_retries":            s.MaxRetries,
	} {
		if !v.IsNull() && !v.IsUnknown() && v.ValueInt64() < 1 {
			diags.AddAttributeError(base.AtName(name),
				"Value must be at least 1",
				fmt.Sprintf("`%s` must be at least 1; the API treats 0 as unset and replaces it with its default.", name))
		}
	}
}

func (r *logExportResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan logExportResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	orgID := r.resolveOrgID(plan.OrganizationID)
	exportType := valueOr(plan.ExportType, defaultExportType)
	plan.OrganizationID = types.StringValue(orgID)
	plan.ExportType = types.StringValue(exportType)

	// 1. Configure the destination (required attribute, always present).
	destResp, err := r.putDestination(ctx, orgID, exportType, plan.Destination)
	if err != nil {
		resp.Diagnostics.AddError("Failed to configure export destination", err.Error())
		r.saveErrorState(ctx, &resp.State, &resp.Diagnostics, plan)
		return
	}
	currentEnabled := destResp.Enabled

	// 2. Apply settings (optional).
	if plan.Settings != nil {
		if err := r.patchSettings(ctx, orgID, exportType, plan.Settings); err != nil {
			resp.Diagnostics.AddError("Failed to update export settings", err.Error())
			r.saveErrorState(ctx, &resp.State, &resp.Diagnostics, plan)
			return
		}
	}

	// 3. Reconcile the enabled state (start requires a healthy destination).
	desired := plan.Enabled.ValueBool()
	if err := r.reconcileEnabled(ctx, orgID, exportType, desired, currentEnabled); err != nil {
		resp.Diagnostics.AddError("Failed to set export enabled state", err.Error())
		r.saveErrorState(ctx, &resp.State, &resp.Diagnostics, plan)
		return
	}

	// Everything applied — refresh from the server for an authoritative result.
	state := plan
	if err := r.readInto(ctx, &state); err != nil {
		resp.Diagnostics.AddError("Failed to read export after create", err.Error())
		// The mutations succeeded; track the resource so a re-apply reconciles.
		nullifyUnknownComputed(&plan)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *logExportResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state logExportResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.readInto(ctx, &state); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			// The export no longer exists; drop it so Terraform plans a recreate.
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read export", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *logExportResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var plan, state logExportResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	orgID := r.resolveOrgID(plan.OrganizationID)
	exportType := valueOr(plan.ExportType, defaultExportType)
	plan.OrganizationID = types.StringValue(orgID)
	plan.ExportType = types.StringValue(exportType)

	// Track the server's enabled state as we go so the start/pause reconcile
	// at the end compares against the truth, not a stale assumption.
	currentEnabled := state.Enabled.ValueBool()

	// 1. Destination changes.
	if !destinationEqual(plan.Destination, state.Destination) {
		destResp, err := r.putDestination(ctx, orgID, exportType, plan.Destination)
		if err != nil {
			resp.Diagnostics.AddError("Failed to update export destination", err.Error())
			r.saveErrorState(ctx, &resp.State, &resp.Diagnostics, plan)
			return
		}
		currentEnabled = destResp.Enabled
	}

	// 2. Settings changes.
	if plan.Settings != nil && !settingsEqual(plan.Settings, state.Settings) {
		if err := r.patchSettings(ctx, orgID, exportType, plan.Settings); err != nil {
			resp.Diagnostics.AddError("Failed to update export settings", err.Error())
			r.saveErrorState(ctx, &resp.State, &resp.Diagnostics, plan)
			return
		}
	}

	// 3. Enabled transitions.
	desired := plan.Enabled.ValueBool()
	if err := r.reconcileEnabled(ctx, orgID, exportType, desired, currentEnabled); err != nil {
		resp.Diagnostics.AddError("Failed to set export enabled state", err.Error())
		r.saveErrorState(ctx, &resp.State, &resp.Diagnostics, plan)
		return
	}

	newState := plan
	if err := r.readInto(ctx, &newState); err != nil {
		resp.Diagnostics.AddError("Failed to read export after update", err.Error())
		nullifyUnknownComputed(&plan)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *logExportResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.requireClient(&resp.Diagnostics) {
		return
	}

	var state logExportResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	orgID := r.resolveOrgID(state.OrganizationID)
	exportType := valueOr(state.ExportType, defaultExportType)

	// Pause first so streaming stops before the destination disappears.
	if err := r.postAction(ctx, orgID, exportType, "pause"); err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Failed to pause export", err.Error())
		return
	}

	if err := r.deleteDestination(ctx, orgID, exportType); err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Failed to delete export destination", err.Error())
		return
	}
}

func (r *logExportResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Accept "{orgId}" or "{orgId}/{exportType}".
	id := strings.TrimSpace(req.ID)
	orgID := id
	exportType := defaultExportType
	if before, after, found := strings.Cut(id, "/"); found {
		orgID = before
		if after != "" {
			exportType = after
		}
	}
	if orgID == "" {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			`Expected "{organization_id}" or "{organization_id}/{export_type}".`,
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("organization_id"), orgID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("export_type"), exportType)...)
}

// --- API interaction helpers -------------------------------------------------

func (r *logExportResource) requireClient(diags *diag.Diagnostics) bool {
	if r.client == nil {
		diags.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return false
	}
	return true
}

func (r *logExportResource) resolveOrgID(v types.String) string {
	if !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		return v.ValueString()
	}
	return r.client.OrganizationID()
}

func exportPath(orgID, exportType string, suffix ...string) string {
	p := "exports/" + orgID + "/" + exportType
	if len(suffix) > 0 {
		p += "/" + strings.Join(suffix, "/")
	}
	return p
}

func (r *logExportResource) putDestination(ctx context.Context, orgID, exportType string, d *destinationModel) (*destinationResponse, error) {
	reqBody := buildDestinationRequest(d)
	var out destinationResponse
	if err := r.doJSON(ctx, http.MethodPut, exportPath(orgID, exportType, "destination"), reqBody, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *logExportResource) patchSettings(ctx context.Context, orgID, exportType string, s *settingsModel) error {
	body, err := buildSettingsRequest(ctx, s)
	if err != nil {
		return err
	}
	return r.doJSON(ctx, http.MethodPatch, exportPath(orgID, exportType, "settings"), body, nil)
}

func (r *logExportResource) deleteDestination(ctx context.Context, orgID, exportType string) error {
	return r.doJSON(ctx, http.MethodDelete, exportPath(orgID, exportType, "destination"), nil, nil)
}

func (r *logExportResource) postAction(ctx context.Context, orgID, exportType, action string) error {
	return r.doJSON(ctx, http.MethodPost, exportPath(orgID, exportType, action), nil, nil)
}

// reconcileEnabled drives the export to the desired enabled state, issuing
// start or pause only when the current state differs.
func (r *logExportResource) reconcileEnabled(ctx context.Context, orgID, exportType string, desired, current bool) error {
	switch {
	case desired && !current:
		return r.postAction(ctx, orgID, exportType, "start")
	case !desired && current:
		return r.postAction(ctx, orgID, exportType, "pause")
	default:
		return nil
	}
}

// readInto refreshes state from the server. It reads the export (settings,
// enabled, runtime) and the destination, then maps both onto the model,
// preserving config-only fields (the access keys are never returned).
func (r *logExportResource) readInto(ctx context.Context, state *logExportResourceModel) error {
	orgID := r.resolveOrgID(state.OrganizationID)
	exportType := valueOr(state.ExportType, defaultExportType)

	var exp exportResponse
	if err := r.doJSON(ctx, http.MethodGet, exportPath(orgID, exportType), nil, &exp); err != nil {
		return err
	}

	var dest destinationResponse
	if err := r.doJSON(ctx, http.MethodGet, exportPath(orgID, exportType, "destination"), nil, &dest); err != nil {
		return err
	}

	return mapServerToState(ctx, state, orgID, &exp, &dest)
}

func (r *logExportResource) doJSON(ctx context.Context, method, path string, body, out any) error {
	return doJSON(ctx, r.client, method, path, body, out)
}

// saveErrorState persists best-known state alongside an error so a subsequent
// apply reconciles rather than orphaning a server-side change. It refreshes
// from the server so the recorded state reflects which steps actually took
// effect — this is what lets the next apply re-run the failed step and skip the
// ones that succeeded. If the export no longer exists, no state is written so a
// retry recreates it; if the refresh itself fails, a nulled copy of the model
// is written so the resource is at least tracked.
func (r *logExportResource) saveErrorState(ctx context.Context, dst *tfsdk.State, diags *diag.Diagnostics, model logExportResourceModel) {
	refreshed := model
	if err := r.readInto(ctx, &refreshed); err != nil {
		if isNotFound(err) {
			return
		}
		nullifyUnknownComputed(&model)
		diags.Append(dst.Set(ctx, &model)...)
		return
	}
	diags.Append(dst.Set(ctx, &refreshed)...)
}

// --- request/response DTOs ---------------------------------------------------

// configureDestinationRequest mirrors the SMS PUT .../destination body.
type configureDestinationRequest struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	PathPrefix      string `json:"path_prefix,omitempty"`
	UseSSL          bool   `json:"use_ssl"`
	UsePathStyle    bool   `json:"use_path_style"`
	AuthMethod      string `json:"auth_method,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	IAMRoleArn      string `json:"iam_role_arn,omitempty"`
}

// updateSettingsRequest mirrors the SMS PATCH .../settings body. Only the
// fields this resource manages are sent; unset pointers are left untouched.
type updateSettingsRequest struct {
	BatchSize            *int64    `json:"batch_size,omitempty"`
	FlushIntervalSeconds *int64    `json:"flush_interval_seconds,omitempty"`
	MaxRetries           *int64    `json:"max_retries,omitempty"`
	IncludedEventTypes   *[]string `json:"included_event_types,omitempty"`
}

// destinationDTO mirrors the SMS state.ExportDestination JSON.
type destinationDTO struct {
	Endpoint       string `json:"endpoint,omitempty"`
	Region         string `json:"region,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	PathPrefix     string `json:"path_prefix,omitempty"`
	UseSSL         bool   `json:"use_ssl,omitempty"`
	UsePathStyle   bool   `json:"use_path_style,omitempty"`
	HasCredentials bool   `json:"has_credentials,omitempty"`
	AuthMethod     string `json:"auth_method,omitempty"`
	IAMRoleArn     string `json:"iam_role_arn,omitempty"`
	ExternalID     string `json:"external_id,omitempty"`
}

// settingsDTO mirrors the SMS state.ExportSettings JSON (fields not managed by
// this resource are decoded but ignored).
type settingsDTO struct {
	BatchSize            int64    `json:"batch_size"`
	FlushIntervalSeconds int64    `json:"flush_interval_seconds"`
	MaxRetries           int64    `json:"max_retries"`
	IncludedEventTypes   []string `json:"included_event_types,omitempty"`
}

// exportResponse mirrors the SMS GET .../{exportType} response (subset).
type exportResponse struct {
	OrganizationID string         `json:"organization_id"`
	ExportType     string         `json:"export_type"`
	Enabled        bool           `json:"enabled"`
	Settings       settingsDTO    `json:"settings"`
	Destination    destinationDTO `json:"destination"`
}

// destinationResponse mirrors the SMS destination endpoint response.
type destinationResponse struct {
	OrganizationID string         `json:"organization_id"`
	ExportType     string         `json:"export_type"`
	Enabled        bool           `json:"enabled"`
	Destination    destinationDTO `json:"destination"`
}

// buildDestinationRequest converts the model to the PUT body. Access keys are
// sent only for access_keys auth; the role ARN only for iam_role auth.
func buildDestinationRequest(d *destinationModel) configureDestinationRequest {
	authMethod := valueOr(d.AuthMethod, authMethodAccessKeys)
	req := configureDestinationRequest{
		Endpoint:     d.Endpoint.ValueString(),
		Region:       d.Region.ValueString(),
		Bucket:       d.Bucket.ValueString(),
		PathPrefix:   d.PathPrefix.ValueString(),
		UseSSL:       d.UseSSL.ValueBool(),
		UsePathStyle: d.UsePathStyle.ValueBool(),
		AuthMethod:   authMethod,
	}
	if authMethod == authMethodIAMRole {
		req.IAMRoleArn = d.IAMRoleArn.ValueString()
	} else {
		req.AccessKeyID = d.AccessKeyID.ValueString()
		req.SecretAccessKey = d.SecretAccessKey.ValueString()
	}
	return req
}

// buildSettingsRequest converts the model to the PATCH body, sending only
// attributes that are known (set by config or carried from prior state).
func buildSettingsRequest(ctx context.Context, s *settingsModel) (updateSettingsRequest, error) {
	var body updateSettingsRequest
	if v, ok := knownInt64(s.BatchSize); ok {
		body.BatchSize = &v
	}
	if v, ok := knownInt64(s.FlushIntervalSeconds); ok {
		body.FlushIntervalSeconds = &v
	}
	if v, ok := knownInt64(s.MaxRetries); ok {
		body.MaxRetries = &v
	}
	if !s.IncludedEventTypes.IsNull() && !s.IncludedEventTypes.IsUnknown() {
		eventTypes := []string{}
		if diags := s.IncludedEventTypes.ElementsAs(ctx, &eventTypes, false); diags.HasError() {
			return body, fmt.Errorf("read included_event_types: %v", diags)
		}
		body.IncludedEventTypes = &eventTypes
	}
	return body, nil
}

// mapServerToState overlays the server's view onto state. Config-only fields
// (access_key_id, secret_access_key) are preserved from the prior model since
// the API never returns them. included_event_types is left untouched when the
// server reports none, so an empty-vs-null distinction in config is preserved.
func mapServerToState(ctx context.Context, state *logExportResourceModel, orgID string, exp *exportResponse, dest *destinationResponse) error {
	state.OrganizationID = types.StringValue(orgID)
	state.ExportType = types.StringValue(valueOrString(exp.ExportType, valueOr(state.ExportType, defaultExportType)))
	state.Enabled = types.BoolValue(exp.Enabled)

	d := dest.Destination
	if d.Endpoint == "" && d.Bucket == "" {
		// No customer destination configured.
		state.Destination = nil
	} else {
		prev := state.Destination
		next := &destinationModel{
			Endpoint:       types.StringValue(d.Endpoint),
			Region:         stringOrNull(d.Region),
			Bucket:         types.StringValue(d.Bucket),
			PathPrefix:     stringOrNull(d.PathPrefix),
			UseSSL:         types.BoolValue(d.UseSSL),
			UsePathStyle:   types.BoolValue(d.UsePathStyle),
			AuthMethod:     types.StringValue(resolvedAuthMethod(d.AuthMethod)),
			IAMRoleArn:     stringOrNull(d.IAMRoleArn),
			ExternalID:     stringOrNull(d.ExternalID),
			HasCredentials: types.BoolValue(d.HasCredentials),
			// Carried from config/prior state — never returned by the API.
			AccessKeyID:     types.StringNull(),
			SecretAccessKey: types.StringNull(),
		}
		if prev != nil {
			next.AccessKeyID = prev.AccessKeyID
			next.SecretAccessKey = prev.SecretAccessKey
		}
		state.Destination = next
	}

	if state.Settings != nil {
		s := state.Settings
		s.BatchSize = types.Int64Value(exp.Settings.BatchSize)
		s.FlushIntervalSeconds = types.Int64Value(exp.Settings.FlushIntervalSeconds)
		s.MaxRetries = types.Int64Value(exp.Settings.MaxRetries)
		if len(exp.Settings.IncludedEventTypes) > 0 {
			list, diags := types.ListValueFrom(ctx, types.StringType, exp.Settings.IncludedEventTypes)
			if diags.HasError() {
				return fmt.Errorf("map included_event_types: %v", diags)
			}
			s.IncludedEventTypes = list
		} else if s.IncludedEventTypes.IsUnknown() {
			// Created with the field unset and the server has none: settle on null.
			s.IncludedEventTypes = types.ListNull(types.StringType)
		}
	}

	return nil
}

// --- model comparison --------------------------------------------------------

func destinationEqual(a, b *destinationModel) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Endpoint.Equal(b.Endpoint) &&
		a.Region.Equal(b.Region) &&
		a.Bucket.Equal(b.Bucket) &&
		a.PathPrefix.Equal(b.PathPrefix) &&
		a.UseSSL.Equal(b.UseSSL) &&
		a.UsePathStyle.Equal(b.UsePathStyle) &&
		a.AuthMethod.Equal(b.AuthMethod) &&
		a.IAMRoleArn.Equal(b.IAMRoleArn) &&
		a.AccessKeyID.Equal(b.AccessKeyID) &&
		a.SecretAccessKey.Equal(b.SecretAccessKey)
}

func settingsEqual(a, b *settingsModel) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.BatchSize.Equal(b.BatchSize) &&
		a.FlushIntervalSeconds.Equal(b.FlushIntervalSeconds) &&
		a.MaxRetries.Equal(b.MaxRetries) &&
		a.IncludedEventTypes.Equal(b.IncludedEventTypes)
}

// nullifyUnknownComputed replaces any unknown computed value with null so the
// model can be written to state (which forbids unknown values).
func nullifyUnknownComputed(m *logExportResourceModel) {
	if m.OrganizationID.IsUnknown() {
		m.OrganizationID = types.StringNull()
	}
	if m.ExportType.IsUnknown() {
		m.ExportType = types.StringValue(defaultExportType)
	}
	if m.Enabled.IsUnknown() {
		m.Enabled = types.BoolValue(false)
	}
	if m.Destination != nil {
		d := m.Destination
		nullUnknownString(&d.Region)
		nullUnknownString(&d.PathPrefix)
		nullUnknownString(&d.IAMRoleArn)
		nullUnknownString(&d.AccessKeyID)
		nullUnknownString(&d.SecretAccessKey)
		nullUnknownString(&d.ExternalID)
		if d.UseSSL.IsUnknown() {
			d.UseSSL = types.BoolValue(true)
		}
		if d.UsePathStyle.IsUnknown() {
			d.UsePathStyle = types.BoolValue(false)
		}
		if d.AuthMethod.IsUnknown() {
			d.AuthMethod = types.StringValue(authMethodAccessKeys)
		}
		if d.HasCredentials.IsUnknown() {
			d.HasCredentials = types.BoolNull()
		}
	}
	if m.Settings != nil {
		s := m.Settings
		nullUnknownInt64(&s.BatchSize)
		nullUnknownInt64(&s.FlushIntervalSeconds)
		nullUnknownInt64(&s.MaxRetries)
		if s.IncludedEventTypes.IsUnknown() {
			s.IncludedEventTypes = types.ListNull(types.StringType)
		}
	}
}

// --- small value helpers -----------------------------------------------------

func valueOr(v types.String, fallback string) string {
	if v.IsNull() || v.IsUnknown() || v.ValueString() == "" {
		return fallback
	}
	return v.ValueString()
}

func valueOrString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func stringOrNull(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

func resolvedAuthMethod(s string) string {
	if s == "" {
		return authMethodAccessKeys
	}
	return s
}

func knownInt64(v types.Int64) (int64, bool) {
	if v.IsNull() || v.IsUnknown() {
		return 0, false
	}
	return v.ValueInt64(), true
}

func isNullKnown(v types.String) bool { return v.IsNull() && !v.IsUnknown() }
func isSetKnown(v types.String) bool  { return !v.IsNull() && !v.IsUnknown() }
func nullUnknownString(v *types.String) {
	if v.IsUnknown() {
		*v = types.StringNull()
	}
}
func nullUnknownInt64(v *types.Int64) {
	if v.IsUnknown() {
		*v = types.Int64Null()
	}
}
