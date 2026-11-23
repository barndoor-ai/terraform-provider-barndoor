// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const statusTestResource = "barndoor_dlp_mcp_policy.test"

// dlpMcpStatusConfig renders one policy; an empty status leaves the attribute
// unset.
func dlpMcpStatusConfig(name, servers, status string) string {
	statusLine := ""
	if status != "" {
		statusLine = fmt.Sprintf("  status         = %q\n", status)
	}
	return fmt.Sprintf(`
resource "barndoor_dlp_mcp_policy" "test" {
  name           = %q
  mcp_server_ids = %s
  rules          = []
%s}
`, name, servers, statusLine)
}

// checkWritesSince asserts the policy writes the fake saw after the first
// `from` of them, in order.
func checkWritesSince(fake *fakeDlpMcpServer, from *int, want ...string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		all := fake.writes()
		got := all[*from:]
		if !slices.Equal(got, want) {
			return fmt.Errorf("policy writes = %v, want %v", got, want)
		}
		*from = len(all)
		return nil
	}
}

func TestDlpMcpPolicyResource_statusCreateDraftThenActivateWithContent(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPoliciesV2(true)
	seen := 0

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: dlpMcpStatusConfig("tf-status-draft", `["github"]`, "DRAFT"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(statusTestResource, "status", "DRAFT"),
					checkWritesSince(fake, &seen, "POST status=DRAFT"),
				),
			},
			{
				// Content first, then the status alone, so the activation's
				// conflict check sees the new servers.
				Config: dlpMcpStatusConfig("tf-status-draft", `["github", "jira"]`, "ACTIVE"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(statusTestResource, "status", "ACTIVE"),
					resource.TestCheckResourceAttr(statusTestResource, "mcp_server_ids.#", "2"),
					checkWritesSince(fake, &seen, "PUT content", "PUT status=ACTIVE"),
				),
			},
		},
	})
}

func TestDlpMcpPolicyResource_statusActiveInLifecycleDisabledOrgSendsNoStatus(t *testing.T) {
	fake := setupDlpMcpTest(t)
	seen := 0

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: dlpMcpStatusConfig("tf-status-flagoff", `["github"]`, "ACTIVE"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(statusTestResource, "status", "ACTIVE"),
					checkWritesSince(fake, &seen, "POST status=none"),
				),
			},
			{
				Config: dlpMcpStatusConfig("tf-status-flagoff", `["github", "jira"]`, "ACTIVE"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(statusTestResource, "status", "ACTIVE"),
					checkWritesSince(fake, &seen, "PUT content"),
				),
			},
		},
	})
}

func TestDlpMcpPolicyResource_statusUnsetCreatesActive(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPoliciesV2(true)
	seen := 0

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{{
			Config: dlpMcpStatusConfig("tf-status-unset", `["github"]`, ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(statusTestResource, "status", "ACTIVE"),
				checkWritesSince(fake, &seen, "POST status=none"),
			),
		}},
	})
}

func TestDlpMcpPolicyResource_statusDeactivateWithContentSendsStatusFirst(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPoliciesV2(true)
	seen := 0

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: dlpMcpStatusConfig("tf-status-off", `["github"]`, "ACTIVE"),
				Check:  checkWritesSince(fake, &seen, "POST status=none"),
			},
			{
				Config: dlpMcpStatusConfig("tf-status-off", `["github", "jira"]`, "INACTIVE"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(statusTestResource, "status", "INACTIVE"),
					checkWritesSince(fake, &seen, "PUT status=INACTIVE", "PUT content"),
				),
			},
			{
				// A status-only change sends no content PUT.
				Config: dlpMcpStatusConfig("tf-status-off", `["github", "jira"]`, "ACTIVE"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(statusTestResource, "status", "ACTIVE"),
					checkWritesSince(fake, &seen, "PUT status=ACTIVE"),
				),
			},
		},
	})
}

func TestDlpMcpPolicyResource_statusUnsetLeavesUIChangeAlone(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPoliciesV2(true)
	config := dlpMcpStatusConfig("tf-status-drift", `["github"]`, "")
	seen := 0
	var id string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					id = s.RootModule().Resources[statusTestResource].Primary.ID
					seen = len(fake.writes())
					return nil
				},
			},
			{
				// Deactivated in the app: no diff, no write, state follows.
				PreConfig: func() { fake.setPolicyStatus(t, id, "INACTIVE") },
				Config:    config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(statusTestResource, "status", "INACTIVE"),
					checkWritesSince(fake, &seen),
				),
			},
		},
	})
}

func TestDlpMcpPolicyResource_statusConfiguredIsRestoredAfterUIChange(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPoliciesV2(true)
	config := dlpMcpStatusConfig("tf-status-restore", `["github"]`, "ACTIVE")
	seen := 0
	var id string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					id = s.RootModule().Resources[statusTestResource].Primary.ID
					seen = len(fake.writes())
					return nil
				},
			},
			{
				PreConfig:          func() { fake.setPolicyStatus(t, id, "INACTIVE") },
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(statusTestResource, "status", "ACTIVE"),
					checkWritesSince(fake, &seen, "PUT status=ACTIVE"),
				),
			},
		},
	})
}

func TestDlpMcpPolicyResource_statusConfigErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		policiesV2 bool
		status     string
		want       string
	}{
		"ARCHIVED is not configurable":   {true, "ARCHIVED", `ARCHIVED is not a configurable\s+status`},
		"unknown value":                  {true, "PAUSED", `Invalid policy status`},
		"INACTIVE is not a create value": {true, "INACTIVE", `Cannot create a policy as INACTIVE`},
		"DRAFT needs policy lifecycle":   {false, "DRAFT", `Policy lifecycle is not enabled for this\s+organization`},
	} {
		t.Run(name, func(t *testing.T) {
			fake := setupDlpMcpTest(t)
			fake.setPoliciesV2(tc.policiesV2)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{{
					Config:      dlpMcpStatusConfig("tf-status-bad", `["github"]`, tc.status),
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
			if n := len(fake.policies); n != 0 {
				t.Errorf("%d policies were created despite the error", n)
			}
		})
	}
}

func TestDlpMcpPolicyResource_statusDraftToInactiveIsRefused(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPoliciesV2(true)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{Config: dlpMcpStatusConfig("tf-status-matrix", `["github"]`, "DRAFT")},
			{
				Config:      dlpMcpStatusConfig("tf-status-matrix", `["github"]`, "INACTIVE"),
				ExpectError: regexp.MustCompile(`Status change not allowed`),
			},
			// Via ACTIVE it works.
			{Config: dlpMcpStatusConfig("tf-status-matrix", `["github"]`, "ACTIVE")},
			{
				Config: dlpMcpStatusConfig("tf-status-matrix", `["github"]`, "INACTIVE"),
				Check:  resource.TestCheckResourceAttr(statusTestResource, "status", "INACTIVE"),
			},
		},
	})
}

func TestDlpMcpPolicyResource_statusActivationConflictNamesTheOtherPolicy(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPoliciesV2(true)
	two := func(status string) string {
		return fmt.Sprintf(`
resource "barndoor_dlp_mcp_policy" "first" {
  name           = "tf-status-first"
  mcp_server_ids = ["github"]
  rules          = []
}

resource "barndoor_dlp_mcp_policy" "second" {
  name           = "tf-status-second"
  mcp_server_ids = ["github"]
  rules          = []
  status         = %q
  depends_on     = [barndoor_dlp_mcp_policy.first]
}
`, status)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				// A DRAFT policy may list a server another policy owns.
				Config: two("DRAFT"),
				Check:  resource.TestCheckResourceAttr("barndoor_dlp_mcp_policy.second", "status", "DRAFT"),
			},
			{
				Config:      two("ACTIVE"),
				ExpectError: regexp.MustCompile(`Cannot activate`),
			},
		},
	})
}

func TestDlpMcpPolicyResource_statusConfiguredArchivedOutsideTerraformIsRecreated(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPoliciesV2(true)
	config := dlpMcpStatusConfig("tf-status-gone", `["github"]`, "ACTIVE")
	var firstID string

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: func(s *terraform.State) error {
					firstID = s.RootModule().Resources[statusTestResource].Primary.ID
					return nil
				},
			},
			{
				PreConfig: func() { fake.markPolicyArchived(t, firstID) },
				Config:    config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(statusTestResource, "status", "ACTIVE"),
					func(s *terraform.State) error {
						if id := s.RootModule().Resources[statusTestResource].Primary.ID; id == firstID {
							return fmt.Errorf("the archived policy %s was not replaced", id)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestDlpMcpPolicyResource_statusImportReadsServerStatus(t *testing.T) {
	fake := setupDlpMcpTest(t)
	fake.setPoliciesV2(true)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllDlpMcpPoliciesDeleted(fake),
		Steps: []resource.TestStep{
			{Config: dlpMcpStatusConfig("tf-status-import", `["github"]`, "DRAFT")},
			{
				ResourceName:      statusTestResource,
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}
