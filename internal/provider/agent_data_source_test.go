// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	frameworkdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// --- fake registry list endpoints -------------------------------------------------
//
// Extends fakeRegistryServer (mcp_server_resource_test.go / agent_resource_test.go)
// with the paginated GET list surface the data sources bind: bd-pagination's
// `{"data": [...], "pagination": {...}}` envelope, `search` substring
// narrowing, and page/limit paging.

// registryPageParams parses the page/limit query parameters with the
// server-side defaults.
func registryPageParams(r *http.Request) (page, limit int) {
	page, limit = 1, 10
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v >= 1 {
		page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v >= 1 && v <= 100 {
		limit = v
	}
	return page, limit
}

// writeRegistryPage renders one page of rows in the registry pagination
// envelope.
func writeRegistryPage(w http.ResponseWriter, rows []any, page, limit int) {
	total := len(rows)
	pages := 0
	if total > 0 {
		pages = (total + limit - 1) / limit
	}
	start := (page - 1) * limit
	end := min(start+limit, total)
	data := []any{}
	if start < total {
		data = rows[start:end]
	}

	var nextPage any
	if page < pages {
		nextPage = page + 1
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"pagination": map[string]any{
			"page":      page,
			"limit":     limit,
			"total":     total,
			"pages":     pages,
			"next_page": nextPage,
		},
	})
}

// listAgents serves GET /api/registry/v1/agents: non-deleted registrations
// whose directory name matches `search` as a case-insensitive substring, in
// deterministic id order. Callers hold f.mu (handleAgents took it).
func (f *fakeRegistryServer) listAgents(w http.ResponseWriter, r *http.Request) {
	search := strings.ToLower(r.URL.Query().Get("search"))

	ids := make([]string, 0, len(f.agents))
	for id := range f.agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	rows := []any{}
	for _, id := range ids {
		a := f.agents[id]
		if a.deleted {
			continue
		}
		dir := fakeAgentDirectories[a.ApplicationDirectoryID]
		if search != "" && !strings.Contains(strings.ToLower(dir.Name), search) {
			continue
		}
		rows = append(rows, a.payload())
	}

	page, limit := registryPageParams(r)
	writeRegistryPage(w, rows, page, limit)
}

// --- schema tests --------------------------------------------------------------

func TestAgentDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewAgentDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_agent"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestAgentDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewAgentDataSource().Schema(context.Background(), frameworkdatasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "name", "application_directory_id",
		"write_confirmations_required", "llm_gateway_enabled", "agent_type",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestAgentDataSource_lookupByID(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_agent" "seed" {
  application_directory_id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
  llm_gateway_enabled      = true
}

data "barndoor_agent" "by_id" {
  id = barndoor_agent.seed.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.barndoor_agent.by_id", "id", "barndoor_agent.seed", "id"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent.by_id", "name", "Internal Test Agent"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent.by_id", "agent_type", "internal"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent.by_id", "llm_gateway_enabled", "true"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent.by_id", "write_confirmations_required", "true"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent.by_id", "application_directory_id",
						"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
				),
			},
		},
	})
}

func TestAgentDataSource_lookupByName(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_agent" "seed" {
  application_directory_id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
}

data "barndoor_agent" "by_name" {
  # Reference through the resource so the lookup happens after the apply.
  name = barndoor_agent.seed.name
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.barndoor_agent.by_name", "id", "barndoor_agent.seed", "id"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent.by_name", "name", "Internal Test Agent"),
				),
			},
		},
	})
}

func TestAgentDataSource_nameNotFound(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_agent" "missing" {
  name = "No Such Agent"
}
`,
				ExpectError: regexp.MustCompile(`No live AI Agent registration named`),
			},
		},
	})
}

func TestAgentDataSource_ambiguousName(t *testing.T) {
	setupRegistryTest(t)

	// Two live registrations whose directory entries share the display name
	// "External Test Agent".
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_agent" "dcr" {
  application_directory_id = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
}

resource "barndoor_agent" "dup" {
  application_directory_id = "cccccccc-cccc-cccc-cccc-cccccccccccc"
}

data "barndoor_agent" "ambiguous" {
  name       = "External Test Agent"
  depends_on = [barndoor_agent.dcr, barndoor_agent.dup]
}
`,
				ExpectError: regexp.MustCompile(`AI Agent name is ambiguous`),
			},
		},
	})
}

func TestAgentDataSource_exactlyOneLookupKey(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_agent" "none" {}
`,
				ExpectError: regexp.MustCompile(`Missing Attribute Configuration|Invalid Attribute Combination`),
			},
			{
				Config: `
data "barndoor_agent" "both" {
  id   = "aaaa0000-0000-0000-0000-000000000001"
  name = "Internal Test Agent"
}
`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination`),
			},
		},
	})
}
