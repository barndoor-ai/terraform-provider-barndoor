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
				// unpublish) and must NOT touch the server — in particular it
				// must never delete it, which would tear down its connections.
				Config: activeServerConfig(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(serverName, "published_at", fakePublishedAt),
					func(*terraform.State) error {
						if fake.serverPublishedAt(t, serverID) == nil {
							return fmt.Errorf("destroying the publication unpublished server %s", serverID)
						}
						if fake.serverDeleted(t, serverID) {
							return fmt.Errorf("destroying the publication deleted server %s", serverID)
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

// checkResourceAbsent fails when name is still present in state — the
// discriminating assertion for a Read that must RemoveResource.
func checkResourceAbsent(name string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		if _, ok := s.RootModule().Resources[name]; ok {
			return fmt.Errorf("%s is still in state; the refresh should have dropped it", name)
		}
		return nil
	}
}

// TestMcpServerPublicationResource_repointForcesReplace pins that changing
// mcp_server_id REPLACES the publication (publishing the new server) rather
// than updating in place. Without the RequiresReplace plan modifier this
// routes to Update, which must fail loudly rather than report a server as
// published without ever calling the publish endpoint.
func TestMcpServerPublicationResource_repointForcesReplace(t *testing.T) {
	fake := setupRegistryTest(t)
	const pubName = "barndoor_mcp_server_publication.test"

	// Two active servers, each with a policy, so either may be published.
	twoServers := `
resource "barndoor_mcp_server" "a" {
  name                    = "tf-test-repoint-a"
  mcp_server_directory_id = "11111111-1111-1111-1111-111111111111"
  client_id               = "tenant-client-id"
}

resource "barndoor_mcp_server" "b" {
  name                    = "tf-test-repoint-b"
  mcp_server_directory_id = "11111111-1111-1111-1111-111111111111"
  client_id               = "tenant-client-id"
}
`
	pubFor := func(ref string) string {
		return fmt.Sprintf(`
resource "barndoor_mcp_server_publication" "test" {
  mcp_server_id = barndoor_mcp_server.%s.id
}
`, ref)
	}

	var idA, idB string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: twoServers,
				Check: func(s *terraform.State) error {
					idA = s.RootModule().Resources["barndoor_mcp_server.a"].Primary.ID
					idB = s.RootModule().Resources["barndoor_mcp_server.b"].Primary.ID
					fake.grantActivePolicy(t, idA)
					fake.grantActivePolicy(t, idB)
					return nil
				},
			},
			{
				Config: twoServers + pubFor("a"),
				Check:  resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAt),
			},
			{
				// Repointing must replace, which publishes server B. If this
				// ever routed to Update, B would be reported published while
				// the publish endpoint was never called for it.
				Config: twoServers + pubFor("b"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(pubName, "mcp_server_id", "barndoor_mcp_server.b", "id"),
					func(*terraform.State) error {
						if fake.serverPublishedAt(t, idB) == nil {
							return fmt.Errorf("repointing did not publish server B (%s)", idB)
						}
						// A stays published — publishing is one-way.
						if fake.serverPublishedAt(t, idA) == nil {
							return fmt.Errorf("repointing unpublished server A (%s)", idA)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestMcpServerPublicationResource_unpublishedOutOfBandRepublishes covers
// Read's "server reads back unpublished" branch: the publication is dropped
// from state so the next apply republishes.
func TestMcpServerPublicationResource_unpublishedOutOfBandRepublishes(t *testing.T) {
	fake := setupRegistryTest(t)
	const pubName = "barndoor_mcp_server_publication.test"

	var serverID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: activeServerConfig(""),
				Check: func(s *terraform.State) error {
					serverID = s.RootModule().Resources["barndoor_mcp_server.test"].Primary.ID
					fake.grantActivePolicy(t, serverID)
					return nil
				},
			},
			{
				Config: activeServerConfig(publicationBlock),
				Check:  resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAt),
			},
			{
				// The row reads back unpublished (replaced/restored
				// out-of-band). The refresh must DROP the publication.
				PreConfig:          func() { fake.unpublishServer(t, serverID) },
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check:              checkResourceAbsent(pubName),
			},
			{
				// …and the next apply republishes it.
				Config: activeServerConfig(publicationBlock),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAt),
					func(*terraform.State) error {
						if fake.serverPublishedAt(t, serverID) == nil {
							return fmt.Errorf("server %s was not republished", serverID)
						}
						return nil
					},
				),
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
				// Out-of-band server delete: the refresh must drop the
				// publication itself (not merely fail later because the server
				// was recreated) — assert its absence from refreshed state.
				PreConfig:          func() { fake.markServerDeleted(t, serverID) },
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check:              checkResourceAbsent(pubName),
			},
			{
				// Applying then recreates the server, and the publish must
				// FAIL on the fresh, policy-less server: a recreated server
				// must never silently republish without its policy.
				Config:      activeServerConfig(publicationBlock),
				ExpectError: regexp.MustCompile(`(?s)cannot be published yet`),
			},
		},
	})
}
