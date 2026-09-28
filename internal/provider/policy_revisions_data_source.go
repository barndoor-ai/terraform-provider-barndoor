// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// Ensure the data source satisfies the framework interfaces it relies on.
var (
	_ datasource.DataSource                   = &policyRevisionsDataSource{}
	_ datasource.DataSourceWithConfigure      = &policyRevisionsDataSource{}
	_ datasource.DataSourceWithValidateConfig = &policyRevisionsDataSource{}
)

// policyRevisionsPath is the policy-service public REST endpoint that lists an
// MCP server's policy revisions. Unlike the policy resource and data source,
// which use the gRPC SDK, this endpoint is REST-only.
const policyRevisionsPath = "api/policy/v2/policy-revisions"

// policyRevisionsPageLimit is the page size requested from the endpoint (its
// maximum), so a read makes as few round trips as possible.
const policyRevisionsPageLimit = 100

// NewPolicyRevisionsDataSource returns a new barndoor_policy_revisions data
// source.
func NewPolicyRevisionsDataSource() datasource.DataSource {
	return &policyRevisionsDataSource{}
}

// policyRevisionsDataSource lists the revision history of the policies that
// govern one MCP server, newest first.
type policyRevisionsDataSource struct {
	client *client.Client
}

// policyRevisionsDataSourceModel maps the data source schema to Go types.
type policyRevisionsDataSourceModel struct {
	ID          types.String          `tfsdk:"id"`
	McpServerID types.String          `tfsdk:"mcp_server_id"`
	Since       types.String          `tfsdk:"since"`
	Revisions   []policyRevisionModel `tfsdk:"revisions"`
}

// policyRevisionModel is one element of the `revisions` list.
type policyRevisionModel struct {
	ID              types.String `tfsdk:"id"`
	PolicyID        types.String `tfsdk:"policy_id"`
	VersionNumber   types.Int64  `tfsdk:"version_number"`
	RevisedAt       types.String `tfsdk:"revised_at"`
	RevisedBy       types.String `tfsdk:"revised_by"`
	RevisedByName   types.String `tfsdk:"revised_by_name"`
	TriggeredBy     types.String `tfsdk:"triggered_by"`
	TriggeredByName types.String `tfsdk:"triggered_by_name"`
	Categories      []string     `tfsdk:"categories"`
	ChangesSummary  []string     `tfsdk:"changes_summary"`
	ChangesJSON     types.String `tfsdk:"changes_json"`
}

// policyRevisionSummaryResponse is the subset of the platform's
// `PolicyRevisionSummary` the data source binds (`resolved_refs` is
// deliberately not read).
type policyRevisionSummaryResponse struct {
	ID              string          `json:"id"`
	PolicyID        string          `json:"policy_id"`
	Changes         json.RawMessage `json:"changes"`
	RevisedAt       string          `json:"revised_at"`
	RevisedBy       string          `json:"revised_by"`
	RevisedByName   *string         `json:"revised_by_name"`
	TriggeredBy     *string         `json:"triggered_by"`
	TriggeredByName *string         `json:"triggered_by_name"`
	Categories      []string        `json:"categories"`
	ChangesSummary  []string        `json:"changes_summary"`
	VersionNumber   int64           `json:"version_number"`
}

// policyRevisionsPage mirrors `ServerPolicyRevisionsPage`.
type policyRevisionsPage struct {
	Data       []policyRevisionSummaryResponse `json:"data"`
	NextCursor *string                         `json:"next_cursor"`
}

func (d *policyRevisionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_policy_revisions"
}

func (d *policyRevisionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the revision history of the policies that govern an MCP server, newest " +
			"first — who changed what, and when. The data source follows the API's pagination to the end, so " +
			"`revisions` holds every matching revision; use `since` to bound a long history. The list is " +
			"scoped to the organization the provider credential belongs to. Requires a Barndoor platform " +
			"release that includes the `/api/policy/v2/policy-revisions` endpoint.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Identifier of the data source; equal to `mcp_server_id`.",
				Computed:            true,
			},
			"mcp_server_id": schema.StringAttribute{
				MarkdownDescription: "UUID of the MCP server whose policy revisions to list.",
				Required:            true,
			},
			"since": schema.StringAttribute{
				MarkdownDescription: "Only return revisions made at or after this instant (inclusive), as an " +
					"RFC 3339 timestamp such as `2026-09-01T00:00:00Z`. Values with a UTC offset are " +
					"converted to UTC before they are sent. Omit to list the full history.",
				Optional: true,
			},
			"revisions": schema.ListNestedAttribute{
				MarkdownDescription: "The policy revisions, newest first (the API's order is preserved).",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							MarkdownDescription: "UUID of the revision.",
							Computed:            true,
						},
						"policy_id": schema.StringAttribute{
							MarkdownDescription: "UUID of the policy that was revised.",
							Computed:            true,
						},
						"version_number": schema.Int64Attribute{
							MarkdownDescription: "Sequential version number of the policy after this revision.",
							Computed:            true,
						},
						"revised_at": schema.StringAttribute{
							MarkdownDescription: "When the revision was made, as an RFC 3339 timestamp in UTC " +
								"(for example `2026-09-28T18:10:24.830776Z`).",
							Computed: true,
						},
						"revised_by": schema.StringAttribute{
							MarkdownDescription: "Identifier of the principal that made the revision.",
							Computed:            true,
						},
						"revised_by_name": schema.StringAttribute{
							MarkdownDescription: "Display name of the principal that made the revision; null " +
								"when the API has none.",
							Computed: true,
						},
						"triggered_by": schema.StringAttribute{
							MarkdownDescription: "Identifier of the principal or system event that triggered " +
								"the revision, when it was not a direct edit; null otherwise.",
							Computed: true,
						},
						"triggered_by_name": schema.StringAttribute{
							MarkdownDescription: "Display name of the principal or system event that triggered " +
								"the revision; null when the API has none.",
							Computed: true,
						},
						"categories": schema.ListAttribute{
							MarkdownDescription: "Categories of change the revision touched (an empty list when " +
								"the API reports none).",
							ElementType: types.StringType,
							Computed:    true,
						},
						"changes_summary": schema.ListAttribute{
							MarkdownDescription: "Human-readable one-line summaries of the changes in the " +
								"revision (an empty list when the API reports none).",
							ElementType: types.StringType,
							Computed:    true,
						},
						"changes_json": schema.StringAttribute{
							MarkdownDescription: "The revision's full `changes` object as compact JSON. Decode " +
								"it with `jsondecode()`; its shape depends on what changed and is not a stable " +
								"contract, so prefer `changes_summary` for display.",
							Computed: true,
						},
					},
				},
			},
		},
	}
}

// ValidateConfig rejects a `since` that is not RFC 3339 at plan time, so a typo
// surfaces before any API call.
func (d *policyRevisionsDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var since types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("since"), &since)...)
	if resp.Diagnostics.HasError() || since.IsNull() || since.IsUnknown() {
		return
	}
	if _, err := parseSince(since.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("since"), "Invalid `since` timestamp", err.Error())
	}
}

func (d *policyRevisionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *policyRevisionsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data policyRevisionsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	since := ""
	if !data.Since.IsNull() {
		t, err := parseSince(data.Since.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("since"), "Invalid `since` timestamp", err.Error())
			return
		}
		since = t.UTC().Format(time.RFC3339Nano)
	}

	rows, err := listPolicyRevisions(ctx, d.client, data.McpServerID.ValueString(), since)
	if err != nil {
		resp.Diagnostics.AddError("Failed to list policy revisions", err.Error())
		return
	}

	revisions := make([]policyRevisionModel, 0, len(rows))
	for _, row := range rows {
		m, err := policyRevisionModelFromResponse(row)
		if err != nil {
			resp.Diagnostics.AddError("Failed to read policy revision",
				fmt.Sprintf("revision %s: %s", row.ID, err))
			return
		}
		revisions = append(revisions, m)
	}

	data.ID = data.McpServerID
	data.Revisions = revisions
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// listPolicyRevisions fetches every revision of serverID (optionally bounded
// below by since, an RFC 3339 UTC timestamp), following next_cursor until the
// API reports no further page. API order (newest first) is preserved. A cursor
// the server hands back twice is a pagination bug on its side, and following
// it would loop forever, so it is an error.
func listPolicyRevisions(ctx context.Context, c *client.Client, serverID, since string) ([]policyRevisionSummaryResponse, error) {
	var (
		all    []policyRevisionSummaryResponse
		cursor string
		seen   = map[string]struct{}{}
	)
	for {
		q := url.Values{}
		q.Set("mcp_server_id", serverID)
		q.Set("limit", strconv.Itoa(policyRevisionsPageLimit))
		if since != "" {
			q.Set("since", since)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}

		var page policyRevisionsPage
		if err := doJSON(ctx, c, http.MethodGet, policyRevisionsPath+"?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)

		if page.NextCursor == nil || *page.NextCursor == "" {
			return all, nil
		}
		next := *page.NextCursor
		if _, dup := seen[next]; dup {
			return nil, fmt.Errorf("GET %s: the API returned the pagination cursor %q more than once; "+
				"stopping rather than looping forever (this is a Barndoor API bug)", policyRevisionsPath, next)
		}
		seen[next] = struct{}{}
		cursor = next
	}
}

// policyRevisionModelFromResponse maps one API row to the schema model.
func policyRevisionModelFromResponse(r policyRevisionSummaryResponse) (policyRevisionModel, error) {
	revisedAt, err := parseRevisedAt(r.RevisedAt)
	if err != nil {
		return policyRevisionModel{}, err
	}

	changesJSON := types.StringNull()
	if trimmed := bytes.TrimSpace(r.Changes); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		var buf bytes.Buffer
		if err := json.Compact(&buf, trimmed); err != nil {
			return policyRevisionModel{}, fmt.Errorf("compact changes: %w", err)
		}
		changesJSON = types.StringValue(buf.String())
	}

	return policyRevisionModel{
		ID:              types.StringValue(r.ID),
		PolicyID:        types.StringValue(r.PolicyID),
		VersionNumber:   types.Int64Value(r.VersionNumber),
		RevisedAt:       types.StringValue(revisedAt.UTC().Format(time.RFC3339Nano)),
		RevisedBy:       types.StringValue(r.RevisedBy),
		RevisedByName:   optionalStringFromPtr(r.RevisedByName, types.StringNull()),
		TriggeredBy:     optionalStringFromPtr(r.TriggeredBy, types.StringNull()),
		TriggeredByName: optionalStringFromPtr(r.TriggeredByName, types.StringNull()),
		Categories:      nonNilStrings(r.Categories),
		ChangesSummary:  nonNilStrings(r.ChangesSummary),
		ChangesJSON:     changesJSON,
	}, nil
}

// parseSince parses the user-supplied `since` (RFC 3339, offset required).
func parseSince(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a valid RFC 3339 timestamp (for example "+
			"\"2026-09-01T00:00:00Z\" or \"2026-09-01T09:00:00-07:00\"): %w", s, err)
	}
	return t, nil
}

// naiveTimestampLayout is the layout of a timezone-less ISO timestamp with
// optional fractional seconds.
const naiveTimestampLayout = "2006-01-02T15:04:05.999999999"

// parseRevisedAt parses the API's `revised_at`. The platform serializes it as
// a NAIVE ISO timestamp (no Z or offset) that is UTC wall-clock, so a value
// without an offset is read as UTC; an offset-bearing value is honoured.
func parseRevisedAt(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.ParseInLocation(naiveTimestampLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("revised_at %q is not a valid ISO 8601 timestamp: %w", s, err)
	}
	return t, nil
}
