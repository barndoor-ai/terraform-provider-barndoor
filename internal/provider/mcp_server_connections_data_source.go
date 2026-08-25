// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the data source satisfies the framework interfaces it relies on.
var (
	_ datasource.DataSource              = &mcpServerConnectionsDataSource{}
	_ datasource.DataSourceWithConfigure = &mcpServerConnectionsDataSource{}
)

// serverConnectionStatuses are the statuses a connection row can actually hold
// in the registry database, and so the only values the roster endpoint accepts
// as a `status` filter.
//
// `available` is deliberately absent. The platform computes it per-caller from
// OpenBao credential state and never persists it, so the endpoint rejects it
// with a 400. Validating here turns that into a plan-time error naming the
// accepted values, rather than an apply-time API failure.
var serverConnectionStatuses = []string{"connected", "error", "pending"}

// serverConnectionOwnerClasses are the mutually exclusive owner classes a
// connection can belong to: a person (`user_id` set), an AI agent
// (`application_id` set), or the tenant service account (neither).
var serverConnectionOwnerClasses = []string{"agent", "service_account", "user"}

// NewMcpServerConnectionsDataSource returns a new
// barndoor_mcp_server_connections data source.
func NewMcpServerConnectionsDataSource() datasource.DataSource {
	return &mcpServerConnectionsDataSource{}
}

// mcpServerConnectionsDataSource reads the org-admin connection roster for one
// MCP server through the registry-service public REST API
// (`GET /api/registry/v1/servers/{server_id}/connections`) — who in the
// organization currently holds a connection to it.
//
// It is the inverse of the per-user connection view: the endpoint requires the
// admin-only `list_connections` Cerbos action, because the roster discloses
// which members use which connector.
type mcpServerConnectionsDataSource struct {
	client *client.Client
}

// mcpServerConnectionsDataSourceModel maps the data source schema to Go types.
type mcpServerConnectionsDataSourceModel struct {
	ServerID    types.String `tfsdk:"server_id"`
	Status      types.Set    `tfsdk:"status"`
	Owner       types.Set    `tfsdk:"owner"`
	Connections types.List   `tfsdk:"connections"`
}

// serverConnectionResponse is the registry's `ServerConnectionSummary` read
// model. The owner fields are mutually exclusive and both are nullable, so they
// are pointers — a service-account row has neither set.
type serverConnectionResponse struct {
	ConnectionID   string  `json:"connection_id"`
	UserID         *string `json:"user_id"`
	ApplicationID  *string `json:"application_id"`
	Status         string  `json:"status"`
	CreatedAt      string  `json:"created_at"`
	ConnectedAt    *string `json:"connected_at"`
	LastAccessedAt *string `json:"last_accessed_at"`
}

// serverConnectionAttrTypes is the object type of one element of the
// `connections` list.
var serverConnectionAttrTypes = map[string]attr.Type{
	"connection_id":    types.StringType,
	"user_id":          types.StringType,
	"application_id":   types.StringType,
	"status":           types.StringType,
	"created_at":       types.StringType,
	"connected_at":     types.StringType,
	"last_accessed_at": types.StringType,
}

func (d *mcpServerConnectionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_server_connections"
}

func (d *mcpServerConnectionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists who in the organization currently holds a connection to an MCP server — " +
			"the inverse of a per-user connection view. Answers \"who do we need to tell\" when a connector " +
			"is being retired or replaced.\n\n" +
			"Unlike audit logs, which show who *used* a connector inside a retention window, this reads " +
			"current connection state: a member who has already migrated and disconnected is simply absent.\n\n" +
			"~> **This writes per-member data into Terraform state.** The roster names which people " +
			"connected which connector and when they last used it. The platform gates the endpoint behind " +
			"an organization-admin permission precisely so members cannot enumerate what their colleagues " +
			"connected — but anyone who can read your state file can read the roster, with no admin role " +
			"and no audit trail. Treat state containing this data source accordingly, and prefer the API " +
			"or CLI for one-off migration sweeps.\n\n" +
			"-> Requires an **organization-admin** credential (the `list_connections` permission on " +
			"`servers`), which is not granted by ordinary read access. A non-admin credential fails with a " +
			"permission error. Requires platform release **v2.30.0** or later.",
		Attributes: map[string]schema.Attribute{
			"server_id": schema.StringAttribute{
				MarkdownDescription: "UUID of the MCP server whose roster to read. A server belonging to " +
					"another organization is reported as not found. A soft-deleted server is still " +
					"readable — \"who holds a connection to the thing we are retiring\" outlives the " +
					"deletion of the server itself.",
				Required: true,
			},
			"status": schema.SetAttribute{
				MarkdownDescription: "Restrict the roster to these connection statuses: `" +
					strings.Join(serverConnectionStatuses, "`, `") + "`. Omit to include every status.\n\n" +
					"How much to trust this field: `error` is a positive dead signal for OAuth connectors, " +
					"because the platform's keepalive sweep refreshes every `connected` OAuth connection on " +
					"a few-hour cycle and flips a credential it cannot refresh to `error`. `connected` is " +
					"unverified for everything else — non-OAuth connectors (api_key, basic_auth, " +
					"bearer_token, plaid, generic) and environments with the sweep disabled, where refresh " +
					"is lazy and a row can sit in `connected` behind a token that no longer works.\n\n" +
					"`available` is not accepted: the platform computes it per-caller and never stores it, " +
					"so filtering on it would always return nothing.",
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.OneOf(serverConnectionStatuses...)),
				},
			},
			"owner": schema.SetAttribute{
				MarkdownDescription: "Restrict the roster to these owner classes: `" +
					strings.Join(serverConnectionOwnerClasses, "`, `") + "`. Omit to include every class, " +
					"which is the default and is almost always what a migration sweep wants.\n\n" +
					"The classes are mutually exclusive: `user` is a person (`user_id` set), `agent` is an " +
					"AI agent (`application_id` set), and `service_account` is the tenant service account " +
					"(neither set). An agent still bound to a server being retired blocks the migration " +
					"just as much as a person does — it is a different remediation, not a row to hide. Set " +
					"`owner = [\"user\"]` only when you specifically want a list of people to notify.",
				ElementType: types.StringType,
				Optional:    true,
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.OneOf(serverConnectionOwnerClasses...)),
				},
			},
			"connections": schema.ListNestedAttribute{
				MarkdownDescription: "Every connection matching the filters, most recently used first " +
					"(connections never used sort last). The whole roster is read — the list is not " +
					"truncated to one API page.",
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"connection_id": schema.StringAttribute{
							MarkdownDescription: "UUID of the connection row.",
							Computed:            true,
						},
						"user_id": schema.StringAttribute{
							MarkdownDescription: "External identity-provider subject of the person who owns " +
								"the connection; null for agent- and service-account-owned rows. This is " +
								"the same value as a user's `external_id` in the identity API, which is " +
								"how it resolves to a name or email — there is no endpoint returning " +
								"users and their connections together.",
							Computed: true,
						},
						"application_id": schema.StringAttribute{
							MarkdownDescription: "UUID of the AI agent that owns the connection, when an " +
								"agent owns it rather than a person; null otherwise.",
							Computed: true,
						},
						"status": schema.StringAttribute{
							MarkdownDescription: "Stored connection status: `" +
								strings.Join(serverConnectionStatuses, "`, `") + "`. See the `status` " +
								"filter above for how far to trust it.",
							Computed: true,
						},
						"created_at": schema.StringAttribute{
							MarkdownDescription: "RFC 3339 timestamp of when the connection row was created.",
							Computed:            true,
						},
						"connected_at": schema.StringAttribute{
							MarkdownDescription: "RFC 3339 timestamp of when the connection was " +
								"established; null while it is still pending.",
							Computed: true,
						},
						"last_accessed_at": schema.StringAttribute{
							MarkdownDescription: "RFC 3339 timestamp of when the connection was last used, " +
								"or null if it never has been.\n\n" +
								"**This is not a liveness signal.** It tracks use, and the keepalive sweep " +
								"never writes it, so a connection that was never used can sit behind a " +
								"credential the sweep refreshes on every cycle with this field still null. " +
								"Do not read null or long-stale as \"connected once and moved on\" — those " +
								"members still hold the connector and still need to be told.",
							Computed: true,
						},
					},
				},
			},
		},
	}
}

func (d *mcpServerConnectionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	d.client = c
}

func (d *mcpServerConnectionsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data mcpServerConnectionsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	query := url.Values{}
	if filter, ok := csvFilter(ctx, data.Status, &resp.Diagnostics); ok && filter != "" {
		query.Set("status", filter)
	}
	if filter, ok := csvFilter(ctx, data.Owner, &resp.Diagnostics); ok && filter != "" {
		query.Set("owner", filter)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	serverID := data.ServerID.ValueString()
	// searchRegistry pages to exhaustion, so the roster is never silently
	// short. The endpoint bounds `limit` at 100; a partial roster is the exact
	// failure mode this data source exists to remove.
	rows, err := searchRegistry(ctx, d.client,
		registryAPIPrefix+"/servers/"+url.PathEscape(serverID)+"/connections",
		query,
		func(serverConnectionResponse) bool { return true },
	)
	if err != nil {
		d.addReadError(&resp.Diagnostics, serverID, err)
		return
	}

	elements := make([]attr.Value, 0, len(rows))
	for _, row := range rows {
		obj, diags := types.ObjectValue(serverConnectionAttrTypes, map[string]attr.Value{
			"connection_id":    types.StringValue(row.ConnectionID),
			"user_id":          types.StringPointerValue(row.UserID),
			"application_id":   types.StringPointerValue(row.ApplicationID),
			"status":           types.StringValue(row.Status),
			"created_at":       types.StringValue(row.CreatedAt),
			"connected_at":     types.StringPointerValue(row.ConnectedAt),
			"last_accessed_at": types.StringPointerValue(row.LastAccessedAt),
		})
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		elements = append(elements, obj)
	}

	list, diags := types.ListValue(types.ObjectType{AttrTypes: serverConnectionAttrTypes}, elements)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.Connections = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// addReadError turns a roster read failure into a diagnostic that names the
// likely cause. A 403 here almost always means the credential is not an
// organization admin, or the target environment predates the endpoint — both
// look identical on the wire, and neither is obvious from the raw API error.
func (d *mcpServerConnectionsDataSource) addReadError(diags *diag.Diagnostics, serverID string, err error) {
	if apiErr, ok := asAPIError(err); ok {
		switch apiErr.status {
		case http.StatusNotFound:
			diags.AddError(
				"MCP server not found",
				fmt.Sprintf("No MCP server with id %q exists in the organization. A server owned by "+
					"another organization is also reported as not found.", serverID),
			)
			return
		case http.StatusForbidden:
			diags.AddError(
				"Not permitted to read the connection roster",
				fmt.Sprintf("Reading the roster for server %q requires an organization-admin credential "+
					"(the `list_connections` permission on `servers`), which ordinary read access does "+
					"not grant. This also fails with a permission error against a platform release "+
					"older than v2.30.0, where the permission does not exist yet.\n\nAPI error: %s",
					serverID, apiErr.Error()),
			)
			return
		}
	}
	diags.AddError("Failed to read the MCP server connection roster", err.Error())
}

// csvFilter renders an optional set of filter values as the comma-separated
// string the endpoint expects, or "" when the filter was omitted. Values are
// sorted so the request is byte-stable across runs regardless of the order the
// set happens to iterate in.
func csvFilter(ctx context.Context, set types.Set, diags *diag.Diagnostics) (string, bool) {
	if set.IsNull() || set.IsUnknown() {
		return "", true
	}
	var values []string
	diags.Append(set.ElementsAs(ctx, &values, false)...)
	if diags.HasError() {
		return "", false
	}
	sort.Strings(values)
	return strings.Join(values, ","), true
}
