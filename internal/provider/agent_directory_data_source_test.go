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

// --- fake registry /agent-directory endpoints --------------------------------------
//
// Extends fakeRegistryServer with the public agent-directory surface the
// barndoor_agent_directory data source binds. The rows enrich the
// fakeAgentDirectories catalog (agent_resource_test.go) — same ids and names,
// so the agent-registration endpoints and these lookups stay one consistent
// world (TestFakeAgentDirectoryRows_matchCatalog pins that).

// fakeAgentDirectoryRows is the full-row view of fakeAgentDirectories, in the
// list endpoint's order (deterministic by id here).
var fakeAgentDirectoryRows = []fakeAgentDirectoryRow{
	{
		ID:           "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		Name:         "Internal Test Agent",
		Description:  dirStr("Org-defined confidential client"),
		OwnerName:    dirStr("Platform Team"),
		OwnerContact: dirStr("platform@acme.example"),
		OrgID:        "org-123",
		ExternalID:   dirStr("internal-test-agent-client"),
		Public:       false,
		AppType:      dirStr("machine_to_machine"),
		DCR:          false,
	},
	{
		ID:          "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		Name:        "External Test Agent",
		Description: dirStr("Dynamically registered client"),
		OrgID:       "org-123",
		ExternalID:  nil, // DCR/multi-client agents have no single upstream client.
		Public:      true,
		DCR:         true,
	},
	{
		ID:          "cccccccc-cccc-cccc-cccc-cccccccccccc",
		Name:        "External Test Agent",
		Description: dirStr("Shares its display name with the entry above"),
		OrgID:       "org-123",
		ExternalID:  dirStr("external-test-agent-client"),
		Public:      false,
		DCR:         false,
	},
}

type fakeAgentDirectoryRow struct {
	ID           string
	Name         string
	Description  *string
	OwnerName    *string
	OwnerContact *string
	OrgID        string
	ExternalID   *string
	Public       bool
	AppType      *string
	DCR          bool
}

// listPayload renders the row as the real list endpoint's transform does: it
// omits app_type and dcr, so they serialize as their model defaults
// (null/false). The data source must not read them from list rows.
func (d *fakeAgentDirectoryRow) listPayload() map[string]any {
	return map[string]any{
		"id":              d.ID,
		"name":            d.Name,
		"description":     d.Description,
		"owner_name":      d.OwnerName,
		"owner_contact":   d.OwnerContact,
		"organization_id": d.OrgID,
		"external_id":     d.ExternalID,
		"public":          d.Public,
		"app_type":        nil,
		"dcr":             false,
		"callbacks":       []string{},
	}
}

// getPayload renders the row as the real GET-by-id endpoint does: the full
// model plus an oauth_config wrapper the data source must ignore.
func (d *fakeAgentDirectoryRow) getPayload() map[string]any {
	p := d.listPayload()
	p["app_type"] = d.AppType
	p["dcr"] = d.DCR
	p["oauth_config"] = map[string]any{
		"callbacks":           []string{},
		"allowed_logout_urls": []string{},
		"app_type":            d.AppType,
	}
	return p
}

// handleAgentDirectory serves GET /api/registry/v1/agent-directory (list) and
// GET /api/registry/v1/agent-directory/{id}.
func (f *fakeRegistryServer) handleAgentDirectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}

	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/registry/v1/agent-directory"), "/")
	if id == "" {
		search := strings.ToLower(r.URL.Query().Get("search"))
		rows := []any{}
		for i := range fakeAgentDirectoryRows {
			d := &fakeAgentDirectoryRows[i]
			desc := ""
			if d.Description != nil {
				desc = *d.Description
			}
			if search != "" &&
				!strings.Contains(strings.ToLower(d.Name), search) &&
				!strings.Contains(strings.ToLower(desc), search) {
				continue
			}
			rows = append(rows, d.listPayload())
		}
		page, limit := registryPageParams(r)
		writeRegistryPage(w, rows, page, limit)
		return
	}

	for i := range fakeAgentDirectoryRows {
		if fakeAgentDirectoryRows[i].ID == id {
			_ = json.NewEncoder(w).Encode(fakeAgentDirectoryRows[i].getPayload())
			return
		}
	}
	writeJSONError(w, http.StatusNotFound, "Application directory not found")
}

// TestFakeAgentDirectoryRows_matchCatalog pins the structural invariant the
// fake relies on: every enriched directory row mirrors the id/name/dcr triple
// in fakeAgentDirectories (which backs agent registration), and vice versa.
func TestFakeAgentDirectoryRows_matchCatalog(t *testing.T) {
	if got, want := len(fakeAgentDirectoryRows), len(fakeAgentDirectories); got != want {
		t.Fatalf("fakeAgentDirectoryRows has %d rows, fakeAgentDirectories has %d", got, want)
	}
	for _, row := range fakeAgentDirectoryRows {
		cat, ok := fakeAgentDirectories[row.ID]
		if !ok {
			t.Errorf("row %s missing from fakeAgentDirectories", row.ID)
			continue
		}
		if row.Name != cat.Name || row.DCR != cat.DCR {
			t.Errorf("row %s = (%q, dcr=%t), catalog = (%q, dcr=%t)",
				row.ID, row.Name, row.DCR, cat.Name, cat.DCR)
		}
	}
}

// --- schema tests --------------------------------------------------------------

func TestAgentDirectoryDataSource_Metadata(t *testing.T) {
	var resp frameworkdatasource.MetadataResponse
	NewAgentDirectoryDataSource().Metadata(context.Background(),
		frameworkdatasource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_agent_directory"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestAgentDirectoryDataSource_Schema(t *testing.T) {
	var resp frameworkdatasource.SchemaResponse
	NewAgentDirectoryDataSource().Schema(context.Background(), frameworkdatasource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	for _, attr := range []string{
		"id", "name", "description", "owner_name", "owner_contact",
		"organization_id", "external_id", "public", "app_type", "dcr",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("schema missing attribute %q", attr)
		}
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func TestAgentDirectoryDataSource_lookupByID(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_agent_directory" "by_id" {
  id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_id", "name", "Internal Test Agent"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_id", "description",
						"Org-defined confidential client"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_id", "owner_name", "Platform Team"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_id", "owner_contact", "platform@acme.example"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_id", "organization_id", "org-123"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_id", "external_id", "internal-test-agent-client"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_id", "public", "false"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_id", "app_type", "machine_to_machine"),
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_id", "dcr", "false"),
				),
			},
		},
	})
}

func TestAgentDirectoryDataSource_idNotFound(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_agent_directory" "missing" {
  id = "aaaaaaaa-0000-0000-0000-000000000099"
}
`,
				ExpectError: regexp.MustCompile(`Agent directory entry not found`),
			},
		},
	})
}

func TestAgentDirectoryDataSource_lookupByName(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_agent_directory" "by_name" {
  name = "Internal Test Agent"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_name", "id",
						"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
					// app_type/dcr only ride the GET-by-id response — their
					// presence proves the lookup resolved to an id and re-read
					// rather than mapping a (degraded) list row.
					resource.TestCheckResourceAttr(
						"data.barndoor_agent_directory.by_name", "app_type", "machine_to_machine"),
				),
			},
		},
	})
}

func TestAgentDirectoryDataSource_ambiguousName(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_agent_directory" "ambiguous" {
  name = "External Test Agent"
}
`,
				ExpectError: regexp.MustCompile(`(?s)Agent directory name is ambiguous.*` +
					`bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb.*cccccccc-cccc-cccc-cccc-cccccccccccc`),
			},
		},
	})
}

func TestAgentDirectoryDataSource_nameNotFound(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_agent_directory" "missing" {
  name = "No Such Agent Directory"
}
`,
				ExpectError: regexp.MustCompile(`No agent directory entry named`),
			},
		},
	})
}

func TestAgentDirectoryDataSource_exactlyOneLookupKey(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_agent_directory" "none" {}
`,
				ExpectError: regexp.MustCompile(`Missing Attribute Configuration|Invalid Attribute Combination`),
			},
			{
				Config: `
data "barndoor_agent_directory" "both" {
  id   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
  name = "Internal Test Agent"
}
`,
				ExpectError: regexp.MustCompile(`Invalid Attribute Combination`),
			},
		},
	})
}

// TestAgentDirectoryDataSource_feedsAgentResource is the end-to-end wiring
// proof: a barndoor_agent resource takes its application_directory_id from
// the data source and the value survives the apply round-trip (the fake's
// register endpoint validates the id against its directory catalog).
func TestAgentDirectoryDataSource_feedsAgentResource(t *testing.T) {
	setupRegistryTest(t)
	resourceName := "barndoor_agent.internal"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
data "barndoor_agent_directory" "internal" {
  name = "Internal Test Agent"
}

resource "barndoor_agent" "internal" {
  application_directory_id = data.barndoor_agent_directory.internal.id
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "application_directory_id",
						"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
					resource.TestCheckResourceAttrPair(
						resourceName, "application_directory_id",
						"data.barndoor_agent_directory.internal", "id"),
				),
			},
		},
	})
}
