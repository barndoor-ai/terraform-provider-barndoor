// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	frameworkdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// --- fake registry /server-directory endpoints -------------------------------------
//
// Extends fakeRegistryServer with the public directory catalog surface the
// barndoor_mcp_server_directory data source binds: the paginated list (search
// narrows by name/description substring like the real handler; there is no
// slug filter) and GET by id. The catalog is a fixed seed — the provider only
// ever reads it.

func dirStr(s string) *string { return &s }

// fakeServerDirectories is the directory catalog the fake serves, in the list
// endpoint's default order (name ascending). It deliberately contains a
// public/org-owned pair sharing the slug "motherduck" (the real schema's
// uniqueness is per (organization_id, slug) only) and a pair sharing the name
// "Custom Tools" (names carry no uniqueness rule at all).
var fakeServerDirectories = []fakeServerDirectory{
	{
		ID:          "dddddddd-0000-0000-0000-000000000004",
		Name:        "Custom Tools",
		Slug:        nil,
		Description: dirStr("First org-created entry"),
		OrgID:       dirStr("org-123"),
		Public:      false,
		Source:      "custom_internal",
		URL:         "https://tools-a.internal.example/mcp",
	},
	{
		ID:          "dddddddd-0000-0000-0000-000000000005",
		Name:        "Custom Tools",
		Slug:        nil,
		Description: dirStr("Second org-created entry with the same name"),
		OrgID:       dirStr("org-123"),
		Public:      false,
		Source:      "custom_internal",
		URL:         "https://tools-b.internal.example/mcp",
	},
	{
		ID:          "dddddddd-0000-0000-0000-000000000001",
		Name:        "GitHub",
		Slug:        dirStr("github"),
		Description: dirStr("GitHub MCP connector"),
		OrgID:       nil,
		Public:      true,
		Source:      "barndoor",
		URL:         "https://api.githubcopilot.com/mcp/",
	},
	{
		ID:          "dddddddd-0000-0000-0000-000000000002",
		Name:        "MotherDuck",
		Slug:        dirStr("motherduck"),
		Description: dirStr("Public MotherDuck connector"),
		OrgID:       nil,
		Public:      true,
		Source:      "barndoor",
		URL:         "https://mcp.motherduck.example/mcp",
	},
	{
		ID:          "dddddddd-0000-0000-0000-000000000003",
		Name:        "MotherDuck (private)",
		Slug:        dirStr("motherduck"),
		Description: dirStr("Org-owned entry reusing the public slug"),
		OrgID:       dirStr("org-123"),
		Public:      false,
		Source:      "custom_third_party",
		URL:         "https://mcp.motherduck.example/mcp",
	},
}

type fakeServerDirectory struct {
	ID          string
	Name        string
	Slug        *string
	Description *string
	OrgID       *string
	Public      bool
	Source      string
	URL         string
}

// payload renders the MCPServerDirectoryRead wire shape (snake_case keys; the
// endpoint's by-alias serialization only affects keys nested inside `meta`).
func (d *fakeServerDirectory) payload() map[string]any {
	return map[string]any{
		"id":              d.ID,
		"name":            d.Name,
		"slug":            d.Slug,
		"description":     d.Description,
		"organization_id": d.OrgID,
		"public":          d.Public,
		"source":          d.Source,
		"url":             d.URL,
		// Connection plumbing the data source must ignore.
		"oauth_metadata":              map[string]any{"issuer": "", "authorization_endpoint": ""},
		"protected_resource_metadata": map[string]any{},
		"scopes":                      []string{},
		"provider":                    "custom",
		"protocol":                    "mcp",
		"requires_auth":               false,
		"categories":                  []any{},
	}
}

// handleServerDirectory serves GET /api/registry/v1/server-directory (list)
// and GET /api/registry/v1/server-directory/{id}.
func (f *fakeRegistryServer) handleServerDirectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/registry/v1/server-directory"), "/")
	if id == "" {
		search := strings.ToLower(r.URL.Query().Get("search"))
		rows := []any{}
		for i := range fakeServerDirectories {
			d := &fakeServerDirectories[i]
			desc := ""
			if d.Description != nil {
				desc = *d.Description
			}
			if search != "" &&
				!strings.Contains(strings.ToLower(d.Name), search) &&
				!strings.Contains(strings.ToLower(desc), search) {
				continue
			}
			rows = append(rows, d.payload())
		}
		page, limit := registryPageParams(r)
		writeRegistryPage(w, rows, page, limit)
		return
	}

	for i := range fakeServerDirectories {
		if fakeServerDirectories[i].ID == id {
			_ = json.NewEncoder(w).Encode(fakeServerDirectories[i].payload())
			return
		}
	}
	writeJSONError(w, http.StatusNotFound, "MCP Server Directory not found")
}

// --- schema tests --------------------------------------------------------------

func TestMcpServerDirectoryDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewMcpServerDirectoryDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_mcp_server_directory"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestMcpServerDirectoryDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewMcpServerDirectoryDataSource().Schema(context.Background(), frameworkdatasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "name", "slug", "description", "organization_id", "public", "source", "url",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestMcpServerDirectoryDataSource_lookupByID(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "by_id" {
  id = "dddddddd-0000-0000-0000-000000000001"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_id", "name", "GitHub"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_id", "slug", "github"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_id", "description", "GitHub MCP connector"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_id", "public", "true"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_id", "source", "barndoor"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_id", "url", "https://api.githubcopilot.com/mcp/"),
					// Public catalog entries have no owning organization.
					resource.TestCheckNoResourceAttr(
						"data.barndoor_mcp_server_directory.by_id", "organization_id"),
				),
			},
		},
	})
}

func TestMcpServerDirectoryDataSource_idNotFound(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "missing" {
  id = "dddddddd-0000-0000-0000-000000000099"
}
`,
				ExpectError: regexp.MustCompile(`MCP server directory entry not found`),
			},
		},
	})
}

func TestMcpServerDirectoryDataSource_lookupBySlug(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "by_slug" {
  slug = "github"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_slug", "id",
						"dddddddd-0000-0000-0000-000000000001"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_slug", "name", "GitHub"),
				),
			},
		},
	})
}

func TestMcpServerDirectoryDataSource_ambiguousSlug(t *testing.T) {
	setupRegistryTest(t)

	// A public entry and an org-owned entry share the slug "motherduck"; the
	// lookup must refuse to pick one, and the diagnostic must carry both ids
	// and the public/org-owned distinction.
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "ambiguous" {
  slug = "motherduck"
}
`,
				ExpectError: regexp.MustCompile(`(?s)MCP server directory slug is ambiguous.*` +
					`dddddddd-0000-0000-0000-000000000002.*public catalog entry.*` +
					`dddddddd-0000-0000-0000-000000000003.*organization-owned`),
			},
		},
	})
}

func TestMcpServerDirectoryDataSource_slugNotFound(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "missing" {
  slug = "no-such-slug"
}
`,
				ExpectError: regexp.MustCompile(`No MCP server directory entry with slug`),
			},
		},
	})
}

func TestMcpServerDirectoryDataSource_lookupByName(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "by_name" {
  name = "MotherDuck (private)"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_name", "id",
						"dddddddd-0000-0000-0000-000000000003"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_name", "slug", "motherduck"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_name", "public", "false"),
					resource.TestCheckResourceAttr(
						"data.barndoor_mcp_server_directory.by_name", "organization_id", "org-123"),
				),
			},
		},
	})
}

func TestMcpServerDirectoryDataSource_ambiguousName(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "ambiguous" {
  name = "Custom Tools"
}
`,
				ExpectError: regexp.MustCompile(`(?s)MCP server directory name is ambiguous.*` +
					`dddddddd-0000-0000-0000-000000000004.*dddddddd-0000-0000-0000-000000000005`),
			},
		},
	})
}

func TestMcpServerDirectoryDataSource_nameNotFound(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "missing" {
  name = "No Such Connector"
}
`,
				ExpectError: regexp.MustCompile(`No MCP server directory entry named`),
			},
		},
	})
}

func TestMcpServerDirectoryDataSource_exactlyOneLookupKey(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "none" {}
`,
				ExpectError: regexp.MustCompile(`Missing Attribute Configuration|Invalid Attribute Combination`),
			},
			{
				Config: `
data "barndoor_mcp_server_directory" "both" {
  slug = "github"
  name = "GitHub"
}
`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination`),
			},
		},
	})
}

// TestMcpServerDirectoryDataSource_feedsMcpServerResource is the end-to-end
// wiring proof: a barndoor_mcp_server resource takes its
// mcp_server_directory_id from the data source and the value survives the
// apply round-trip.
func TestMcpServerDirectoryDataSource_feedsMcpServerResource(t *testing.T) {
	fake := setupRegistryTest(t)
	resourceName := "barndoor_mcp_server.github"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllServersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_mcp_server_directory" "github" {
  slug = "github"
}

resource "barndoor_mcp_server" "github" {
  name                    = "GitHub"
  mcp_server_directory_id = data.barndoor_mcp_server_directory.github.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "mcp_server_directory_id",
						"dddddddd-0000-0000-0000-000000000001"),
					resource.TestCheckResourceAttrPair(
						resourceName, "mcp_server_directory_id",
						"data.barndoor_mcp_server_directory.github", "id"),
				),
			},
		},
	})
}
