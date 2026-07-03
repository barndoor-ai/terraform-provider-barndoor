// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	frameworkdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// --- fake /servers list + by-slug endpoints ---------------------------------------

// listServers serves GET /api/registry/v1/servers: non-deleted servers whose
// name or slug matches `search` as a case-insensitive substring, in
// deterministic id order. Callers hold f.mu (handleServers took it).
func (f *fakeRegistryServer) listServers(w http.ResponseWriter, r *http.Request) {
	search := strings.ToLower(r.URL.Query().Get("search"))

	ids := make([]string, 0, len(f.servers))
	for id := range f.servers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	rows := []any{}
	for _, id := range ids {
		s := f.servers[id]
		if s.deleted {
			continue
		}
		if search != "" &&
			!strings.Contains(strings.ToLower(s.Name), search) &&
			!strings.Contains(strings.ToLower(s.Slug), search) {
			continue
		}
		rows = append(rows, s)
	}

	page, limit := registryPageParams(r)
	writeRegistryPage(w, rows, page, limit)
}

// getServerBySlug serves GET /api/registry/v1/servers/by-slug/{slug}. Callers
// hold f.mu (handleServers took it).
func (f *fakeRegistryServer) getServerBySlug(w http.ResponseWriter, slug string) {
	for _, s := range f.servers {
		if !s.deleted && s.Slug == slug {
			_ = json.NewEncoder(w).Encode(s)
			return
		}
	}
	writeJSONError(w, http.StatusNotFound, "MCP server not found")
}

// --- schema tests --------------------------------------------------------------

func TestMcpServerDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewMcpServerDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_mcp_server"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestMcpServerDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewMcpServerDataSource().Schema(context.Background(), frameworkdatasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "name", "slug", "status", "mcp_server_directory_id",
		"oauth_base_url_override", "uses_managed_credentials", "scopes",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

const mcpServerDataSourceSeed = `
resource "barndoor_mcp_server" "seed" {
  name                    = "Data Source Test Server"
  mcp_server_directory_id = "dir-1"
  scopes                  = ["repo:read"]
}
`

func TestMcpServerDataSource_lookupByID(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: mcpServerDataSourceSeed + `
data "barndoor_mcp_server" "by_id" {
  id = barndoor_mcp_server.seed.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.barndoor_mcp_server.by_id", "id", "barndoor_mcp_server.seed", "id"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server.by_id", "name", "Data Source Test Server"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server.by_id", "slug", "data-source-test-server"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server.by_id", "status", "pending"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server.by_id", "mcp_server_directory_id", "dir-1"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server.by_id", "scopes.#", "1"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server.by_id", "scopes.0", "repo:read"),
					resource.TestCheckNoResourceAttr(
						"data.barndoor_mcp_server.by_id", "oauth_base_url_override"),
				),
			},
		},
	})
}

func TestMcpServerDataSource_lookupByName(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The name deliberately differs from the seeded one in case and
				// surrounding whitespace: matching mirrors the API's uniqueness
				// rule, so this must still resolve.
				Config: mcpServerDataSourceSeed + `
data "barndoor_mcp_server" "by_name" {
  name       = "  data source TEST server "
  depends_on = [barndoor_mcp_server.seed]
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.barndoor_mcp_server.by_name", "id", "barndoor_mcp_server.seed", "id"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server.by_name", "name", "Data Source Test Server"),
				),
			},
		},
	})
}

func TestMcpServerDataSource_lookupBySlug(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: mcpServerDataSourceSeed + `
data "barndoor_mcp_server" "by_slug" {
  slug = barndoor_mcp_server.seed.slug
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"data.barndoor_mcp_server.by_slug", "id", "barndoor_mcp_server.seed", "id"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server.by_slug", "name", "Data Source Test Server"),
				),
			},
		},
	})
}

func TestMcpServerDataSource_notFound(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server" "missing_name" {
  name = "No Such Server"
}
`,
				ExpectError: regexp.MustCompile(`No MCP server named`),
			},
			{
				Config: `
data "barndoor_mcp_server" "missing_id" {
  id = "00000000-0000-0000-0000-999999999999"
}
`,
				ExpectError: regexp.MustCompile(`MCP server not found`),
			},
			{
				Config: `
data "barndoor_mcp_server" "missing_slug" {
  slug = "no-such-slug"
}
`,
				ExpectError: regexp.MustCompile(`MCP server not found`),
			},
		},
	})
}

func TestMcpServerDataSource_exactlyOneLookupKey(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server" "none" {}
`,
				ExpectError: regexp.MustCompile(`Missing Attribute Configuration|Invalid Attribute Combination`),
			},
			{
				Config: `
data "barndoor_mcp_server" "two" {
  name = "x"
  slug = "x"
}
`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination`),
			},
		},
	})
}
