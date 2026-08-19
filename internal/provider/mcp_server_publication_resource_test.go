// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- schema tests --------------------------------------------------------------

func TestMcpServerPublicationResource_Metadata(t *testing.T) {
	var resp frameworkresource.MetadataResponse
	NewMcpServerPublicationResource().Metadata(context.Background(),
		frameworkresource.MetadataRequest{ProviderTypeName: "barndoor"}, &resp)

	if got, want := resp.TypeName, "barndoor_mcp_server_publication"; got != want {
		t.Errorf("TypeName = %q, want %q", got, want)
	}
}

func TestMcpServerPublicationResource_Schema(t *testing.T) {
	var resp frameworkresource.SchemaResponse
	NewMcpServerPublicationResource().Schema(context.Background(), frameworkresource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %+v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatalf("schema failed framework validation: %+v", diags)
	}

	if !resp.Schema.Attributes["mcp_server_id"].IsRequired() {
		t.Error("mcp_server_id should be Required")
	}
	if !resp.Schema.Attributes["published_at"].IsComputed() {
		t.Error("published_at should be Computed")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

// activeServerConfig renders an active (credentialed) server; extra appends
// further blocks, e.g. the publication resource.
func activeServerConfig(extra string) string {
	return mcpServerConfig("tf-test-pub", "\n  client_id = \"tenant-client-id\"\n") + extra
}

const publicationBlock = `
resource "barndoor_mcp_server_publication" "test" {
  mcp_server_id = barndoor_mcp_server.test.id
}
`

func TestMcpServerPublicationResource_lifecycle(t *testing.T) {
	fake := setupRegistryTest(t)
	const pubName = "barndoor_mcp_server_publication.test"
	const serverName = "barndoor_mcp_server.test"

	var serverID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The server alone: unpublished, published_at reads null.
				Config: activeServerConfig(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(serverName, "published_at"),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources[serverName]
						if !ok {
							return fmt.Errorf("%s not in state", serverName)
						}
						serverID = rs.Primary.ID
						fake.grantActivePolicy(t, serverID)
						return nil
					},
				),
			},
			{
				// Publish: the stamp lands on the publication resource. (The
				// server resource and data source pick it up on their next
				// refresh — asserted in the following step — because they are
				// read before the publication applies within this step.)
				Config: activeServerConfig(publicationBlock),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAt),
					resource.TestCheckResourceAttrPair(pubName, "mcp_server_id", serverName, "id"),
				),
			},
			{
				// Re-apply: idempotent (no changes), and the refresh surfaces
				// the publish state on the server resource and data source.
				Config: activeServerConfig(publicationBlock + `
data "barndoor_mcp_server" "test" {
  id = barndoor_mcp_server.test.id
}
`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(serverName, "published_at", fakePublishedAt),
					resource.TestCheckResourceAttr("data.barndoor_mcp_server.test", "published_at", fakePublishedAt),
				),
			},
			{
				// Import by server id.
				Config:                               activeServerConfig(publicationBlock),
				ResourceName:                         pubName,
				ImportState:                          true,
				ImportStateIdFunc:                    func(*terraform.State) (string, error) { return serverID, nil },
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "mcp_server_id",
			},
			{
				// Removing the publication must NOT unpublish (there is no
				// unpublish) and must NOT touch the server.
				Config: activeServerConfig(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(serverName, "published_at", fakePublishedAt),
					func(*terraform.State) error {
						if fake.serverPublishedAt(t, serverID) == nil {
							return fmt.Errorf("destroying the publication unpublished server %s", serverID)
						}
						return nil
					},
				),
			},
			{
				// Re-adding the declaration adopts the existing publication
				// (idempotent re-publish), keeping the original stamp.
				Config: activeServerConfig(publicationBlock),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAt),
				),
			},
		},
	})
}

func TestMcpServerPublicationResource_preconditionsRejected(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// A pending (credential-less) server is not operationally
				// available; the publish must fail with the actionable message.
				Config:      mcpServerConfig("tf-test-pub-pending", "") + publicationBlock,
				ExpectError: regexp.MustCompile(`(?s)cannot be published yet.*not operationally available`),
			},
			{
				// An active server without an ACTIVE policy: the diagnostic
				// must point at the depends_on ordering fix.
				Config:      activeServerConfig(publicationBlock),
				ExpectError: regexp.MustCompile(`(?s)cannot be published yet.*no ACTIVE policy.*depends_on`),
			},
		},
	})
}

func TestMcpServerPublicationResource_outOfBandServerDeleteDropsPublication(t *testing.T) {
	fake := setupRegistryTest(t)
	const pubName = "barndoor_mcp_server_publication.test"

	var serverID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: activeServerConfig(""),
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources["barndoor_mcp_server.test"]
					if !ok {
						return fmt.Errorf("server not in state")
					}
					serverID = rs.Primary.ID
					fake.grantActivePolicy(t, serverID)
					return nil
				},
			},
			{
				Config: activeServerConfig(publicationBlock),
				Check:  resource.TestCheckResourceAttrSet(pubName, "published_at"),
			},
			{
				// Out-of-band server delete: the refresh must drop both the
				// server AND its publication, and the apply recreates both
				// (the new server gets a fresh policy grant via PreConfig's
				// ordering being impossible — so expect the publish to fail on
				// the recreated, policy-less server; that failure IS the
				// correct behavior: a recreated server must not silently
				// republish without its policy).
				PreConfig:   func() { fake.markServerDeleted(t, serverID) },
				Config:      activeServerConfig(publicationBlock),
				ExpectError: regexp.MustCompile(`(?s)cannot be published yet`),
			},
		},
	})
}
