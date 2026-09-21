// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	frameworkresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
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
	// Not Computed: a Computed list would be unknown-after-apply on import
	// instead of a stable null.
	if p := resp.Schema.Attributes["policy_ids"]; !p.IsOptional() || p.IsComputed() {
		t.Error("policy_ids should be Optional and not Computed")
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

// activeServerConfig renders an active (credentialed) server; extra appends
// further blocks, e.g. the publication resource.
func activeServerConfig(extra string) string {
	return mcpServerConfig("tf-test-pub", "\n  client_id = \"tenant-client-id\"\n") + extra
}

// publicationBlock is the publication with no policy_ids;
// publicationBlockWithPolicies renders it with the given ids (the fake has no
// policy resource, so literal ids stand in for barndoor_policy.x.id
// references).
var publicationBlock = publicationBlockWithPolicies()

func publicationBlockWithPolicies(ids ...string) string {
	var policyIDs string
	if len(ids) > 0 {
		quoted := make([]string, len(ids))
		for i, id := range ids {
			quoted[i] = strconv.Quote(id)
		}
		policyIDs = "\n  policy_ids    = [" + strings.Join(quoted, ", ") + "]"
	}
	return fmt.Sprintf(`
resource "barndoor_mcp_server_publication" "test" {
  mcp_server_id = barndoor_mcp_server.test.id%s
}
`, policyIDs)
}

// setupPublicationTest is setupRegistryTest plus a shrunk publish retry, so a
// rejected publish fails in a fraction of a second instead of two minutes.
func setupPublicationTest(t *testing.T) *fakeRegistryServer {
	t.Helper()
	window, interval := publishRetryWindow, publishRetryInterval
	publishRetryWindow, publishRetryInterval = 200*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { publishRetryWindow, publishRetryInterval = window, interval })
	return setupRegistryTest(t)
}

func TestMcpServerPublicationResource_lifecycle(t *testing.T) {
	fake := setupPublicationTest(t)
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
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAtFor(1)),
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
					resource.TestCheckResourceAttr(serverName, "published_at", fakePublishedAtFor(1)),
					resource.TestCheckResourceAttr("data.barndoor_mcp_server.test", "published_at", fakePublishedAtFor(1)),
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
					resource.TestCheckResourceAttr(serverName, "published_at", fakePublishedAtFor(1)),
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
				// (idempotent re-publish), keeping the ORIGINAL stamp. Stamps
				// are per-publish, so a second publish would read
				// fakePublishedAtFor(2) — this assertion fails if the API call
				// is not idempotent.
				Config: activeServerConfig(publicationBlock),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAtFor(1)),
				),
			},
		},
	})
}

func TestMcpServerPublicationResource_preconditionsRejected(t *testing.T) {
	fake := setupPublicationTest(t)

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
				// must point at the policy_ids ordering fix.
				Config:      activeServerConfig(publicationBlock),
				ExpectError: regexp.MustCompile(`(?s)cannot be published yet.*no ACTIVE policy.*policy_ids`),
			},
		},
	})
	// Each rejection above was retried for the window before surfacing —
	// not failed on the first attempt, and not retried forever.
	if n := fake.publishAttemptCount(); n < 4 {
		t.Errorf("publish endpoint called %d time(s) across two rejected steps; precondition 422s should be retried until the window closes", n)
	}
}

// --- precondition retry -------------------------------------------------------------

// TestMcpServerPublicationResource_waitsForPolicyLandingLater covers the
// no-ordering case: the publication is applied while the server still has no
// ACTIVE policy, the policy lands a few attempts later (as when Terraform
// creates both concurrently), and Create converges by retrying. Without the
// retry the first 422 surfaces and this fails.
func TestMcpServerPublicationResource_waitsForPolicyLandingLater(t *testing.T) {
	fake := setupPublicationTest(t)
	fake.grantPolicyOnAttempt = 3
	const pubName = "barndoor_mcp_server_publication.test"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: activeServerConfig(publicationBlock),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAtFor(1)),
					func(*terraform.State) error {
						if n := fake.publishAttemptCount(); n != 3 {
							return fmt.Errorf("publish endpoint called %d time(s), want 3 (two rejections, then success)", n)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestMcpServerPublicationResource_neverPublishesAfterWindow pins the bound:
// no attempt starts once the window has closed. With a 15 ms window and a
// 10 ms interval, the policy landing on the 3rd attempt (~20 ms) must never
// be seen — the retry stops after the 2nd attempt and the server stays
// unpublished. Sleeping the full interval and THEN checking the deadline
// would publish here, one interval late.
func TestMcpServerPublicationResource_neverPublishesAfterWindow(t *testing.T) {
	fake := setupPublicationTest(t)
	publishRetryWindow, publishRetryInterval = 15*time.Millisecond, 10*time.Millisecond
	fake.grantPolicyOnAttempt = 3

	var serverID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: activeServerConfig(""),
				Check: func(s *terraform.State) error {
					serverID = s.RootModule().Resources["barndoor_mcp_server.test"].Primary.ID
					return nil
				},
			},
			{
				Config:      activeServerConfig(publicationBlock),
				ExpectError: regexp.MustCompile(`(?s)cannot be published yet.*no ACTIVE policy`),
			},
		},
	})
	if fake.serverPublishedAt(t, serverID) != nil {
		t.Errorf("server %s was published on attempt %d, after the retry window closed", serverID, fake.publishAttemptCount())
	}
}

// TestMcpServerPublicationResource_nonPreconditionFailsImmediately pins the
// retry's scope: only the two precondition 422s wait. A 404 (or any other
// error) is surfaced after a single attempt.
func TestMcpServerPublicationResource_nonPreconditionFailsImmediately(t *testing.T) {
	fake := setupPublicationTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "barndoor_mcp_server_publication" "test" {
  mcp_server_id = "00000000-0000-0000-0000-0000000000ff"
}
`,
				ExpectError: regexp.MustCompile(`MCP server not found`),
			},
		},
	})
	if n := fake.publishAttemptCount(); n != 1 {
		t.Errorf("publish endpoint called %d time(s) for a 404; only precondition 422s may be retried", n)
	}
}

// --- policy_ids ---------------------------------------------------------------------

// TestMcpServerPublicationResource_policyIDs pins policy_ids' ordering-only
// contract: editing or dropping the list is an in-place update that never
// calls the publish endpoint, and an import reads it back null with no
// follow-up diff.
func TestMcpServerPublicationResource_policyIDs(t *testing.T) {
	fake := setupPublicationTest(t)
	const pubName = "barndoor_mcp_server_publication.test"
	const policyA, policyB = "aaaaaaaa-0000-0000-0000-000000000001", "aaaaaaaa-0000-0000-0000-000000000002"

	var serverID string
	var attemptsAfterPublish int
	// noPublishCall fails if the publish endpoint was called since the
	// original publish — the discriminating assertion for a state-only Update.
	noPublishCall := func(*terraform.State) error {
		if n := fake.publishAttemptCount(); n != attemptsAfterPublish {
			return fmt.Errorf("publish endpoint called %d more time(s); a policy_ids change must not call the API", n-attemptsAfterPublish)
		}
		return nil
	}
	updatesInPlace := resource.ConfigPlanChecks{
		PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(pubName, plancheck.ResourceActionUpdate)},
	}

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
				Config: activeServerConfig(publicationBlockWithPolicies(policyA)),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAtFor(1)),
					resource.TestCheckResourceAttr(pubName, "policy_ids.#", "1"),
					func(*terraform.State) error {
						attemptsAfterPublish = fake.publishAttemptCount()
						return nil
					},
				),
			},
			{
				// Editing the list: in place (no -/+), no API call, stamp kept.
				Config:           activeServerConfig(publicationBlockWithPolicies(policyA, policyB)),
				ConfigPlanChecks: updatesInPlace,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pubName, "policy_ids.#", "2"),
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAtFor(1)),
					noPublishCall,
				),
			},
			{
				// Dropping the list entirely: likewise.
				Config:           activeServerConfig(publicationBlock),
				ConfigPlanChecks: updatesInPlace,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(pubName, "policy_ids.#"),
					noPublishCall,
				),
			},
			{
				// Stop tracking the publication (the server stays published)
				// so it can be imported into this state below.
				Config: activeServerConfig(""),
			},
			{
				// Import: policy_ids comes back null (an import cannot know
				// what the publication was ordered after) …
				Config:             activeServerConfig(publicationBlock),
				ResourceName:       pubName,
				ImportState:        true,
				ImportStateIdFunc:  func(*terraform.State) (string, error) { return serverID, nil },
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					// The persisted state also holds the server.
					for _, st := range states {
						if st.Ephemeral.Type != "barndoor_mcp_server_publication" {
							continue
						}
						if n, ok := st.Attributes["policy_ids.#"]; ok {
							return fmt.Errorf("imported policy_ids has %s element(s), want null", n)
						}
						return nil
					}
					return fmt.Errorf("no barndoor_mcp_server_publication among the %d imported instances", len(states))
				},
			},
			{
				// … and a configuration without policy_ids plans no changes
				// against the imported state.
				Config:   activeServerConfig(publicationBlock),
				PlanOnly: true,
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
// routes to Update — a state-only write — and the new server would be
// reported published without the publish endpoint ever being called for it.
func TestMcpServerPublicationResource_repointForcesReplace(t *testing.T) {
	fake := setupPublicationTest(t)
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
				Check:  resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAtFor(1)),
			},
			{
				// Repointing must replace, which publishes server B. If this
				// ever routed to Update, B would be reported published while
				// the publish endpoint was never called for it.
				Config: twoServers + pubFor("b"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(pubName, "mcp_server_id", "barndoor_mcp_server.b", "id"),
					// B's own stamp (the 2nd publish), not A's carried forward:
					// this is what fails if the replacement never called
					// /publish for B and merely copied prior state.
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAtFor(2)),
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
	fake := setupPublicationTest(t)
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
				Check:  resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAtFor(1)),
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
				// …and the next apply republishes it. A genuinely new publish,
				// so the stamp is the 2nd one — not the dropped state's.
				Config: activeServerConfig(publicationBlock),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(pubName, "published_at", fakePublishedAtFor(2)),
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
	fake := setupPublicationTest(t)
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
