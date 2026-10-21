// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

func TestBuildMcpServerWriteRequest_CredentialMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     types.String
		create   bool
		wantMode *string
		wantUMC  *bool
	}{
		{name: "create sends the mode", mode: types.StringValue("dcr"), create: true, wantMode: ptr("dcr")},
		{name: "create sends auto", mode: types.StringValue("auto"), create: true, wantMode: ptr("auto")},
		{
			name: "managed create carries the flag", mode: types.StringValue("managed"), create: true,
			wantMode: ptr("managed"), wantUMC: ptr(true),
		},
		{
			// The update writes the flag from every payload, so a managed
			// server that does not carry it loses its managed client.
			name: "managed update carries the flag", mode: types.StringValue("managed"), create: false,
			wantUMC: ptr(true),
		},
		{name: "update leaves the mode to applyCredentialModeUpdate", mode: types.StringValue("dcr")},
		{name: "unset sends nothing", mode: types.StringNull(), create: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &mcpServerResourceModel{
				Name:                 types.StringValue("Acme"),
				McpServerDirectoryID: types.StringValue("dir-1"),
				CredentialMode:       tc.mode,
			}
			body, err := buildMcpServerWriteRequest(context.Background(), plan, tc.create)
			if err != nil {
				t.Fatalf("buildMcpServerWriteRequest: %v", err)
			}
			if !equalPtr(body.CredentialMode, tc.wantMode) {
				t.Errorf("credential_mode = %s, want %s", fmtPtr(body.CredentialMode), fmtPtr(tc.wantMode))
			}
			if !equalPtr(body.UsesManagedCredentials, tc.wantUMC) {
				t.Errorf("uses_managed_credentials = %s, want %s",
					fmtPtr(body.UsesManagedCredentials), fmtPtr(tc.wantUMC))
			}
		})
	}
}

func TestApplyCredentialModeUpdate(t *testing.T) {
	// A zero types.Bool or types.String is null.
	for _, tc := range []struct {
		name          string
		plan          types.String
		prior         types.String
		priorUMC      types.Bool
		priorResolved types.String
		wantMode      *string
		wantUMC       *bool
	}{
		// Re-sending an unchanged mode would turn a mode changed outside
		// Terraform back.
		{name: "unchanged is not sent", plan: types.StringValue("dcr"), prior: types.StringValue("dcr")},
		{
			name: "changed is sent", plan: types.StringValue("dcr"), prior: types.StringValue("cimd"),
			wantMode: ptr("dcr"),
		},
		{
			name: "set from unset is sent", plan: types.StringValue("manual"), prior: types.StringNull(),
			wantMode: ptr("manual"),
		},
		{name: "auto is never sent", plan: types.StringValue("auto"), prior: types.StringValue("dcr")},
		{
			name: "removing managed keeps the server managed", plan: types.StringNull(),
			prior: types.StringValue("managed"), priorResolved: types.StringValue("managed"), wantUMC: ptr(true),
		},
		{
			// Report-only drift: an unrelated update does not move a server
			// made managed outside Terraform off its managed client.
			name: "an unchanged mode keeps a managed server managed", plan: types.StringValue("dcr"),
			prior: types.StringValue("dcr"), priorResolved: types.StringValue("managed"), wantUMC: ptr(true),
		},
		{
			name: "a mode switch away from managed does not carry the flag", plan: types.StringValue("dcr"),
			prior: types.StringValue("cimd"), priorResolved: types.StringValue("managed"), wantMode: ptr("dcr"),
		},
		{
			// Removing a configured `uses_managed_credentials = true` clears it.
			name: "a configured flag is not kept", plan: types.StringNull(), prior: types.StringNull(),
			priorUMC: types.BoolValue(true), priorResolved: types.StringValue("managed"),
		},
		{name: "removing another mode sends nothing", plan: types.StringNull(), prior: types.StringValue("dcr")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &mcpServerWriteRequest{}
			applyCredentialModeUpdate(body,
				&mcpServerResourceModel{CredentialMode: tc.plan, UsesManagedCredentials: types.BoolNull()},
				&mcpServerResourceModel{
					CredentialMode:         tc.prior,
					UsesManagedCredentials: tc.priorUMC,
					ResolvedCredentialMode: tc.priorResolved,
				})
			if !equalPtr(body.CredentialMode, tc.wantMode) {
				t.Errorf("credential_mode = %s, want %s", fmtPtr(body.CredentialMode), fmtPtr(tc.wantMode))
			}
			if !equalPtr(body.UsesManagedCredentials, tc.wantUMC) {
				t.Errorf("uses_managed_credentials = %s, want %s",
					fmtPtr(body.UsesManagedCredentials), fmtPtr(tc.wantUMC))
			}
		})
	}
}

func TestApplyMcpServerResponse_CredentialMode(t *testing.T) {
	for _, tc := range []struct {
		name         string
		priorMode    types.String
		priorUMC     types.Bool
		serverMode   *string
		serverUMC    *bool
		wantMode     types.String
		wantResolved types.String
		wantUMC      types.Bool
	}{
		{
			name:      "auto stays auto while the stored mode is reported",
			priorMode: types.StringValue("auto"), priorUMC: types.BoolNull(),
			serverMode: ptr("dcr"),
			wantMode:   types.StringValue("auto"), wantResolved: types.StringValue("dcr"), wantUMC: types.BoolNull(),
		},
		{
			name:      "a flag derived from managed stays null",
			priorMode: types.StringValue("managed"), priorUMC: types.BoolNull(),
			serverMode: ptr("managed"), serverUMC: ptr(true),
			wantMode: types.StringValue("managed"), wantResolved: types.StringValue("managed"),
			wantUMC: types.BoolNull(),
		},
		{
			name:      "a configured flag is read back",
			priorMode: types.StringNull(), priorUMC: types.BoolValue(true),
			serverMode: ptr("managed"), serverUMC: ptr(true),
			wantMode: types.StringNull(), wantResolved: types.StringValue("managed"), wantUMC: types.BoolValue(true),
		},
		{
			name:      "an unset flag stays null on a server made managed outside Terraform",
			priorMode: types.StringValue("dcr"), priorUMC: types.BoolNull(),
			serverMode: ptr("managed"), serverUMC: ptr(true),
			wantMode: types.StringValue("dcr"), wantResolved: types.StringValue("managed"), wantUMC: types.BoolNull(),
		},
		{
			name:      "a platform without the field reads null",
			priorMode: types.StringNull(), priorUMC: types.BoolNull(),
			wantMode: types.StringNull(), wantResolved: types.StringNull(), wantUMC: types.BoolNull(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := &mcpServerResourceModel{
				CredentialMode:         tc.priorMode,
				UsesManagedCredentials: tc.priorUMC,
				Scopes:                 types.ListNull(types.StringType),
			}
			server := &mcpServerResponse{
				ID: "srv-1", CredentialMode: tc.serverMode, UsesManagedCredentials: tc.serverUMC,
			}
			state, err := applyMcpServerResponse(context.Background(), server, prior)
			if err != nil {
				t.Fatalf("applyMcpServerResponse: %v", err)
			}
			if !state.CredentialMode.Equal(tc.wantMode) {
				t.Errorf("credential_mode = %s, want %s", state.CredentialMode, tc.wantMode)
			}
			if !state.ResolvedCredentialMode.Equal(tc.wantResolved) {
				t.Errorf("resolved_credential_mode = %s, want %s", state.ResolvedCredentialMode, tc.wantResolved)
			}
			if !state.UsesManagedCredentials.Equal(tc.wantUMC) {
				t.Errorf("uses_managed_credentials = %s, want %s", state.UsesManagedCredentials, tc.wantUMC)
			}
		})
	}
}

// --- lifecycle (real plan/apply against the fake) ---------------------------------

func credentialModeConfig(name, extra string) string {
	return mcpServerConfig(name, extra) + `
data "barndoor_mcp_server" "read" {
  id = barndoor_mcp_server.test.id
}
`
}

// captureServerID stores the managed server's id into *id.
func captureServerID(id *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["barndoor_mcp_server.test"]
		if !ok {
			return fmt.Errorf("barndoor_mcp_server.test not in state")
		}
		*id = rs.Primary.ID
		return nil
	}
}

func expectResolvedMode(mode string) []statecheck.StateCheck {
	return []statecheck.StateCheck{
		statecheck.ExpectKnownValue("barndoor_mcp_server.test", tfjsonpath.New("resolved_credential_mode"),
			knownvalue.StringExact(mode)),
		statecheck.ExpectKnownValue("data.barndoor_mcp_server.read", tfjsonpath.New("credential_mode"),
			knownvalue.StringExact(mode)),
	}
}

// TestMcpServerResource_credentialModeAuto creates a server with `auto`, which
// the registry stores as the mode it chose. The configured value stays in
// state, so the post-apply plan is empty although the two differ.
func TestMcpServerResource_credentialModeAuto(t *testing.T) {
	fake := setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllServersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config:            credentialModeConfig("tf-test-auto", `  credential_mode = "auto"`),
				ConfigStateChecks: expectResolvedMode("dcr"),
				Check:             resource.TestCheckResourceAttr("barndoor_mcp_server.test", "credential_mode", "auto"),
			},
			{
				// An update cannot name `auto`, so a rename must not send it.
				Config:            credentialModeConfig("tf-test-auto-renamed", `  credential_mode = "auto"`),
				ConfigStateChecks: expectResolvedMode("dcr"),
			},
		},
	})

	if got, want := fake.credentialModeWriteLog(), []string{"auto"}; !slices.Equal(got, want) {
		t.Errorf("credential_mode writes = %v, want %v", got, want)
	}
}

// TestMcpServerResource_credentialModeChanges walks a server through every
// kind of mode change and checks which ones update in place and which force a
// new server.
func TestMcpServerResource_credentialModeChanges(t *testing.T) {
	fake := setupRegistryTest(t)
	const addr = "barndoor_mcp_server.test"

	var firstID, secondID, thirdID string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllServersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config:            credentialModeConfig("tf-test-modes", `  credential_mode = "cimd"`),
				ConfigStateChecks: expectResolvedMode("cimd"),
				Check:             captureServerID(&firstID),
			},
			{
				// cimd -> dcr: the registry registers a client in place.
				Config: credentialModeConfig("tf-test-modes", `  credential_mode = "dcr"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate)},
				},
				ConfigStateChecks: expectResolvedMode("dcr"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPtr(addr, "id", &firstID),
				),
			},
			{
				// Into cimd: an update cannot, so the server is replaced.
				Config: credentialModeConfig("tf-test-modes", `  credential_mode = "cimd"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				ConfigStateChecks: expectResolvedMode("cimd"),
				Check:             captureServerID(&secondID),
			},
			{
				Config: credentialModeConfig("tf-test-modes", `  credential_mode = "managed"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate)},
				},
				ConfigStateChecks: expectResolvedMode("managed"),
				Check:             resource.TestCheckResourceAttrPtr(addr, "id", &secondID),
			},
			{
				Config: credentialModeConfig("tf-test-modes", `
  credential_mode = "manual"
  client_id       = "tenant-client-id"
`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate)},
				},
				ConfigStateChecks: expectResolvedMode("manual"),
				Check:             resource.TestCheckResourceAttrPtr(addr, "id", &secondID),
			},
			{
				// Into dcr from anything but cimd: replaced.
				Config: credentialModeConfig("tf-test-modes", `  credential_mode = "dcr"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(addr, plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				ConfigStateChecks: expectResolvedMode("dcr"),
				Check:             captureServerID(&thirdID),
			},
		},
	})

	if firstID == secondID || secondID == thirdID {
		t.Errorf("replacements must create new servers: ids %s, %s, %s", firstID, secondID, thirdID)
	}
}

// TestMcpServerResource_credentialModeManagedKeepsTheFlag covers the
// registry writing uses_managed_credentials from every update payload: a
// server created with `credential_mode = "managed"` must keep the flag through
// an update that does not send credential_mode, and through removing the
// attribute.
func TestMcpServerResource_credentialModeManagedKeepsTheFlag(t *testing.T) {
	fake := setupRegistryTest(t)
	const addr = "barndoor_mcp_server.test"

	var id string
	flagIsSet := func(*terraform.State) error {
		if got := fake.serverUsesManagedCredentials(t, id); got == nil || !*got {
			return fmt.Errorf("stored uses_managed_credentials = %s, want true", fmtPtr(got))
		}
		return nil
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllServersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config:            credentialModeConfig("tf-test-managed", `  credential_mode = "managed"`),
				ConfigStateChecks: expectResolvedMode("managed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureServerID(&id),
					resource.TestCheckNoResourceAttr(addr, "uses_managed_credentials"),
				),
			},
			{
				Config:            credentialModeConfig("tf-test-managed-renamed", `  credential_mode = "managed"`),
				ConfigStateChecks: expectResolvedMode("managed"),
				Check: resource.ComposeAggregateTestCheckFunc(
					flagIsSet,
					resource.TestCheckNoResourceAttr(addr, "uses_managed_credentials"),
				),
			},
			{
				Config:            credentialModeConfig("tf-test-managed-renamed", ""),
				ConfigStateChecks: expectResolvedMode("managed"),
				Check:             flagIsSet,
			},
		},
	})

	if got, want := fake.credentialModeWriteLog(), []string{"managed"}; !slices.Equal(got, want) {
		t.Errorf("credential_mode writes = %v, want %v", got, want)
	}
}

// TestMcpServerResource_credentialModeChangedOutsideTerraform pins the
// report-only handling of drift: the stored mode shows in
// resolved_credential_mode and the plan stays empty, because switching back
// would delete the server's user connections.
func TestMcpServerResource_credentialModeChangedOutsideTerraform(t *testing.T) {
	fake := setupRegistryTest(t)

	var id string
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             checkAllServersDeleted(fake),
		Steps: []resource.TestStep{
			{
				Config:            credentialModeConfig("tf-test-drift", `  credential_mode = "dcr"`),
				ConfigStateChecks: expectResolvedMode("dcr"),
				Check:             captureServerID(&id),
			},
			{
				PreConfig: func() { fake.setServerCredentialMode(t, id, "manual") },
				Config:    credentialModeConfig("tf-test-drift", `  credential_mode = "dcr"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				ConfigStateChecks: expectResolvedMode("manual"),
			},
			{
				// An unrelated update must not re-send the configured mode.
				Config:            credentialModeConfig("tf-test-drift-renamed", `  credential_mode = "dcr"`),
				ConfigStateChecks: expectResolvedMode("manual"),
			},
		},
	})

	if got, want := fake.credentialModeWriteLog(), []string{"dcr"}; !slices.Equal(got, want) {
		t.Errorf("credential_mode writes = %v, want %v", got, want)
	}
}

func TestMcpServerResource_credentialModeValidateConfig(t *testing.T) {
	setupRegistryTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      mcpServerConfig("tf-test-badmode", `  credential_mode = "static_client"`),
				ExpectError: regexp.MustCompile(`value must be one of`),
			},
			{
				Config:      mcpServerConfig("tf-test-manual", `  credential_mode = "manual"`),
				ExpectError: regexp.MustCompile(`credential_mode "manual" requires client_id`),
			},
			{
				Config: mcpServerConfig("tf-test-clientid", `
  credential_mode = "cimd"
  client_id       = "tenant-client-id"
`),
				ExpectError: regexp.MustCompile(`client_id requires credential_mode "manual"`),
			},
			{
				Config: mcpServerConfig("tf-test-flag", `
  credential_mode          = "dcr"
  uses_managed_credentials = true
`),
				ExpectError: regexp.MustCompile(`uses_managed_credentials conflicts with credential_mode`),
			},
		},
	})
}

func ptr[T any](v T) *T { return &v }

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func fmtPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}
