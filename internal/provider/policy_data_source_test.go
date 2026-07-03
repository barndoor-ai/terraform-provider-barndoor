// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	frameworkdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	policyv2 "github.com/barndoor-ai/barndoor-go-sdk/proto/barndoor/public/policy/v2"
)

// --- fake ListPolicies -------------------------------------------------------------
//
// Extends fakePolicyServer (policy_resource_test.go) with the summary-list RPC
// the data source's by-name lookup binds: `search` substring narrowing
// (case-insensitive, like the API's ilike), a status filter, and page/limit
// paging over deterministically-ordered rows.

func (f *fakePolicyServer) ListPolicies(_ context.Context, req *policyv2.ListPoliciesRequest) (*policyv2.ListPoliciesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	page := int(req.GetPage())
	if page < 1 {
		page = 1
	}
	limit := int(req.GetLimit())
	if limit < 1 {
		limit = 10
	}

	wantStatus := map[policyv2.PolicyStatus]bool{}
	for _, s := range req.GetStatus() {
		wantStatus[s] = true
	}
	search := strings.ToLower(req.GetSearch())

	ids := make([]string, 0, len(f.policies))
	for id := range f.policies {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var all []*policyv2.PolicySummaryItem
	for _, id := range ids {
		p := f.policies[id]
		if len(wantStatus) > 0 && !wantStatus[p.GetStatus()] {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(p.GetName()), search) {
			continue
		}
		all = append(all, &policyv2.PolicySummaryItem{
			Id:          p.GetId(),
			Name:        p.GetName(),
			Status:      p.GetStatus(),
			McpServerId: p.GetMcpServerId(),
		})
	}

	total := len(all)
	start := (page - 1) * limit
	end := min(start+limit, total)
	items := []*policyv2.PolicySummaryItem{}
	if start < total {
		items = all[start:end]
	}

	return &policyv2.ListPoliciesResponse{
		Policies: items,
		Pagination: &policyv2.PaginationMetadata{
			Page:  int32(page),
			Limit: int32(limit),
			Total: int32(total),
		},
	}, nil
}

// --- schema tests --------------------------------------------------------------

func TestPolicyDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewPolicyDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_policy"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestPolicyDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewPolicyDataSource().Schema(context.Background(), frameworkdatasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "name", "mcp_server_id", "description", "support_contact",
		"tags", "application_ids", "status", "rules",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestPolicyDataSource_lookupByID(t *testing.T) {
	fake := setupPolicyTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllPoliciesArchived(fake),
		Steps: []resource.TestStep{
			{
				Config: policyConfigFull("tf-ds-policy", testConditionConfigured) + `
data "barndoor_policy" "by_id" {
  id = barndoor_policy.test.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.barndoor_policy.by_id", "id", "barndoor_policy.test", "id"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "name", "tf-ds-policy"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "mcp_server_id", "mcp-server-1"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "description", "Managed by Terraform"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "support_contact", "governance@example.com"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "status", "ACTIVE"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "tags.#", "2"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "application_ids.#", "2"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "rules.#", "1"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "rules.0.name", "allow search"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "rules.0.effect", "ALLOW"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "rules.0.active", "true"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "rules.0.actions.#", "2"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_id", "rules.0.roles.#", "2"),
					resource.TestCheckResourceAttrWith(
						"data.barndoor_policy.by_id", "rules.0.condition", func(v string) error {
							if !strings.Contains(v, "request.tool == 'search'") {
								return fmt.Errorf("condition %q does not carry the expression", v)
							}
							return nil
						}),
				),
			},
		},
	})
}

func TestPolicyDataSource_lookupByName(t *testing.T) {
	fake := setupPolicyTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllPoliciesArchived(fake),
		Steps: []resource.TestStep{
			{
				Config: policyConfigMinimal("tf-ds-by-name") + `
data "barndoor_policy" "by_name" {
  name = barndoor_policy.test.name
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.barndoor_policy.by_name", "id", "barndoor_policy.test", "id"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_name", "name", "tf-ds-by-name"),
					resource.TestCheckResourceAttr(
						"data.barndoor_policy.by_name", "status", "DRAFT"),
				),
			},
		},
	})
}

func TestPolicyDataSource_nameNotFound(t *testing.T) {
	setupPolicyTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_policy" "missing" {
  name = "no-such-policy"
}
`,
				ExpectError: regexp.MustCompile(`No non-archived policy named`),
			},
		},
	})
}

func TestPolicyDataSource_archivedPolicyErrors(t *testing.T) {
	fake := setupPolicyTest(t)

	// Seed and archive a policy directly in the fake: in Terraform terms it is
	// deleted, so a by-id lookup must fail loudly rather than return it.
	archived := policyv2.PolicyStatus_POLICY_STATUS_ARCHIVED
	fake.mu.Lock()
	fake.policies["11111111-1111-1111-1111-111111111111"] = &policyv2.PolicyDetail{
		Id:          "11111111-1111-1111-1111-111111111111",
		Name:        "archived-policy",
		McpServerId: "mcp-server-1",
		Status:      archived,
	}
	fake.mu.Unlock()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_policy" "archived" {
  id = "11111111-1111-1111-1111-111111111111"
}
`,
				ExpectError: regexp.MustCompile(`Policy is archived`),
			},
			{
				// The by-name path filters archived policies out entirely.
				Config: `
data "barndoor_policy" "archived_by_name" {
  name = "archived-policy"
}
`,
				ExpectError: regexp.MustCompile(`No non-archived policy named`),
			},
		},
	})
}

func TestPolicyDataSource_exactlyOneLookupKey(t *testing.T) {
	setupPolicyTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_policy" "none" {}
`,
				ExpectError: regexp.MustCompile(`Missing Attribute Configuration|Invalid Attribute Combination`),
			},
			{
				Config: `
data "barndoor_policy" "both" {
  id   = "11111111-1111-1111-1111-111111111111"
  name = "x"
}
`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination`),
			},
		},
	})
}
