// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	policyv2 "github.com/barndoor-ai/barndoor-go-sdk/proto/barndoor/public/policy/v2"
	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

// policyListPageLimit is the page size requested from ListPolicies (its
// maximum), so a by-name lookup makes as few RPCs as possible.
const policyListPageLimit = 100

// policyListMaxPages bounds how many pages a by-name lookup will walk. A
// search-narrowed lookup finds its match in the first page in practice; the
// bound only guards against a pagination contract that never converges.
const policyListMaxPages = 100

// Ensure the data source satisfies the framework interfaces it relies on.
var (
	_ datasource.DataSource                     = &policyDataSource{}
	_ datasource.DataSourceWithConfigure        = &policyDataSource{}
	_ datasource.DataSourceWithConfigValidators = &policyDataSource{}
)

// NewPolicyDataSource returns a new barndoor_policy data source.
func NewPolicyDataSource() datasource.DataSource {
	return &policyDataSource{}
}

// policyDataSource looks up an existing access policy by id or name through
// the barndoor.policy.v2 gRPC contract, so pre-existing policies can be
// referenced without being managed by Terraform.
type policyDataSource struct {
	client *client.Client
}

// policyDataSourceModel maps the data source schema to Go types. The fields
// mirror the barndoor_policy resource; rules reuse the resource's nested rule
// model so the two shapes can never drift apart.
type policyDataSourceModel struct {
	ID             types.String      `tfsdk:"id"`
	Name           types.String      `tfsdk:"name"`
	McpServerID    types.String      `tfsdk:"mcp_server_id"`
	Description    types.String      `tfsdk:"description"`
	SupportContact types.String      `tfsdk:"support_contact"`
	Tags           types.Set         `tfsdk:"tags"`
	ApplicationIDs types.Set         `tfsdk:"application_ids"`
	Status         types.String      `tfsdk:"status"`
	Rules          []policyRuleModel `tfsdk:"rules"`
}

func (d *policyDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_policy"
}

func (d *policyDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an existing Barndoor access policy by `id` or by `name`. Use it to " +
			"reference a policy that is not managed by this Terraform configuration, or as the lookup " +
			"step before a `terraform import`. Archived policies are not returned: archival is the " +
			"platform's terminal lifecycle state, equivalent to deletion.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Policy UUID. Exactly one of `id` or `name` must be set.",
				Optional:            true,
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Policy name. Exactly one of `id` or `name` must be set. Names are " +
					"unique among the organization's non-archived policies; the comparison is exact.",
				Optional: true,
				Computed: true,
			},
			"mcp_server_id": schema.StringAttribute{
				MarkdownDescription: "ID of the MCP server the policy governs.",
				Computed:            true,
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Free-form description of the policy.",
				Computed:            true,
			},
			"support_contact": schema.StringAttribute{
				MarkdownDescription: "Contact shown to end users when this policy blocks them.",
				Computed:            true,
			},
			"tags": schema.SetAttribute{
				MarkdownDescription: "Organizational tags attached to the policy.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"application_ids": schema.SetAttribute{
				MarkdownDescription: "IDs of the AI Agents (applications) this policy applies to.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"status": schema.StringAttribute{
				MarkdownDescription: "Lifecycle status: `" + policyStatusDraft + "`, `" + policyStatusActive +
					"`, or `" + policyStatusInactive + "`.",
				Computed: true,
			},
			"rules": schema.ListNestedAttribute{
				MarkdownDescription: "Ordered rules evaluated against tool calls.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							MarkdownDescription: "Rule name.",
							Computed:            true,
						},
						"description": schema.StringAttribute{
							MarkdownDescription: "Free-form description of the rule.",
							Computed:            true,
						},
						"effect": schema.StringAttribute{
							MarkdownDescription: "Whether matching calls are allowed or denied: `ALLOW` or `DENY`.",
							Computed:            true,
						},
						"active": schema.BoolAttribute{
							MarkdownDescription: "Whether the rule is enforced.",
							Computed:            true,
						},
						"actions": schema.ListAttribute{
							MarkdownDescription: "Tool/action names the rule matches (`*` matches all).",
							ElementType:         types.StringType,
							Computed:            true,
						},
						"roles": schema.ListAttribute{
							MarkdownDescription: "Principals the rule matches: `role:<name>`, `group:<name>`, or `*`.",
							ElementType:         types.StringType,
							Computed:            true,
						},
						"condition": schema.StringAttribute{
							MarkdownDescription: "Condition tree as canonical JSON, when the rule carries one.",
							Computed:            true,
							CustomType:          jsontypes.NormalizedType{},
						},
					},
				},
			},
		},
	}
}

// ConfigValidators requires exactly one of the two lookup keys.
func (d *policyDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(
			path.MatchRoot("id"),
			path.MatchRoot("name"),
		),
	}
}

func (d *policyDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *policyDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError(
			"Provider not configured",
			"The Barndoor client is not available. This usually means the provider failed to configure.",
		)
		return
	}

	var data policyDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	conn, err := d.client.GRPCConn(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to connect to the policy API", err.Error())
		return
	}
	pc := policyv2.NewPolicyServiceClient(conn)

	id := data.ID.ValueString()
	if id == "" {
		var ok bool
		id, ok = resolvePolicyIDByName(ctx, pc, data.Name.ValueString(), &resp.Diagnostics)
		if !ok {
			return
		}
	}

	rpcResp, err := pc.GetPolicy(ctx, &policyv2.GetPolicyRequest{PolicyId: id})
	if err != nil {
		if grpcstatus.Code(err) == codes.NotFound {
			resp.Diagnostics.AddError(
				"Policy not found",
				fmt.Sprintf("No policy with id %q exists in the organization. Confirm the id, or look the "+
					"policy up by `name`.", id),
			)
			return
		}
		addPolicyRPCError(&resp.Diagnostics, "read the policy", err)
		return
	}
	detail := rpcResp.GetPolicy()
	if detail == nil {
		resp.Diagnostics.AddError("Malformed policy API response", "GetPolicy returned no policy object.")
		return
	}
	if detail.GetStatus() == policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED {
		// Archival is the platform's terminal state (what `terraform destroy`
		// performs on the resource): an archived policy is a deleted one, its
		// name is freed for reuse, and nothing can reference it meaningfully.
		resp.Diagnostics.AddError(
			"Policy is archived",
			fmt.Sprintf("Policy %q exists but is %s — the platform's terminal lifecycle state, equivalent "+
				"to deletion. It cannot be referenced.", id, policyStatusArchived),
		)
		return
	}

	// A zero-value prior model is all-null, so empty optional fields settle to
	// null — a data source has no configuration to disambiguate against.
	state, err := applyPolicyDetail(ctx, detail, &policyResourceModel{})
	if err != nil {
		resp.Diagnostics.AddError("Failed to map the policy API response", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &policyDataSourceModel{
		ID:             state.ID,
		Name:           state.Name,
		McpServerID:    state.McpServerID,
		Description:    state.Description,
		SupportContact: state.SupportContact,
		Tags:           state.Tags,
		ApplicationIDs: state.ApplicationIDs,
		Status:         state.Status,
		Rules:          state.Rules,
	})...)
}

// resolvePolicyIDByName finds the single non-archived policy named name. The
// ListPolicies `search` narrows server-side by substring; the exact comparison
// happens here. Names are unique among non-archived policies, so more than one
// exact match indicates an API contract change — refuse rather than guess.
func resolvePolicyIDByName(ctx context.Context, pc policyv2.PolicyServiceClient, name string, diags *diag.Diagnostics) (string, bool) {
	want := strings.TrimSpace(name)

	var ids []string
	for page := 1; page <= policyListMaxPages; page++ {
		rpcResp, err := pc.ListPolicies(ctx, &policyv2.ListPoliciesRequest{
			Page:   int32(page),
			Limit:  policyListPageLimit,
			Search: &want,
			// Every non-archived state: archived policies are deleted in
			// Terraform terms and their names are freed for reuse.
			Status: []policyv2.PolicyStatus{
				policyv2.PolicyStatus_POLICY_STATUS_DRAFT,
				policyv2.PolicyStatus_POLICY_STATUS_ACTIVE,
				policyv2.PolicyStatus_POLICY_STATUS_INACTIVE,
			},
		})
		if err != nil {
			addPolicyRPCError(diags, "search policies by name", err)
			return "", false
		}

		for _, p := range rpcResp.GetPolicies() {
			if p.GetName() == want {
				ids = append(ids, p.GetId())
			}
		}

		pg := rpcResp.GetPagination()
		if pg == nil || int64(page)*int64(pg.GetLimit()) >= int64(pg.GetTotal()) || len(rpcResp.GetPolicies()) == 0 {
			break
		}
	}

	switch len(ids) {
	case 0:
		diags.AddError(
			"Policy not found",
			fmt.Sprintf("No non-archived policy named %q exists in the organization. The comparison is "+
				"exact (case-sensitive); confirm the name, or look the policy up by `id`.", want),
		)
		return "", false
	case 1:
		return ids[0], true
	default:
		diags.AddError(
			"Policy name is ambiguous",
			fmt.Sprintf("%d non-archived policies are named %q (ids: %s). This should not happen — policy "+
				"names are unique among non-archived policies. Use `id` to disambiguate.",
				len(ids), want, strings.Join(ids, ", ")),
		)
		return "", false
	}
}
