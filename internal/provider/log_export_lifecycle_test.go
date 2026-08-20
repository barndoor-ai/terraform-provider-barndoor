// Copyright Barndoor AI, Inc. 2026
// SPDX-License-Identifier: MIT

package provider

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// Lifecycle tests for barndoor_log_export, driven by real Terraform against the
// in-process fake in log_export_fake_test.go.
//
// These live in their own file because they import
// terraform-plugin-testing/helper/resource as `resource`, which collides with
// the framework's `resource` package used by the schema-level unit tests.
//
// What they prove that the unit tests structurally cannot: "Provider produced
// inconsistent result after apply" is raised by Terraform CORE, by comparing
// the applied value against the planned one. No amount of asserting on
// mapServerToState's output can catch it — only a real plan/apply can. The same
// goes for a perpetual diff, which is what the PlanOnly steps pin.

const logExportResourceName = "barndoor_log_export.test"

// TestLogExportResource_iamRoleToAzureLifecycle is the regression test for the
// external_id transition hazard.
//
// Setup: an S3 destination on iam_role auth, whose sts:ExternalId the API has
// minted — so state holds a KNOWN external_id. The practitioner then switches
// the destination to azure_blob, where no external ID exists and the provider
// maps it to null.
//
// external_id is Computed with no plan modifier, so the concern is that the
// proposed plan carries the prior known value forward while apply produces
// null, which Terraform core rejects. Whether that actually happens is decided
// by the framework's plan pipeline, not by reading the mapper — this test is
// what settles it, and what keeps it settled.
func TestLogExportResource_iamRoleToAzureLifecycle(t *testing.T) {
	fake := setupExportTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Create on iam_role: the API mints and returns the external ID,
				// so it lands in state as a known value.
				Config: logExportIAMRoleConfig(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(logExportResourceName, "destination.provider", storageProviderS3),
					resource.TestCheckResourceAttr(logExportResourceName, "destination.auth_method", authMethodIAMRole),
					resource.TestCheckResourceAttr(logExportResourceName, "destination.external_id", fakeExportExternalID),
					resource.TestCheckResourceAttr(logExportResourceName, "destination.region", "us-east-1"),
				),
			},
			{
				// Steady state: no provider/auth change, and external_id must
				// stay a known value rather than going "(known after apply)".
				Config:   logExportIAMRoleConfig(),
				PlanOnly: true,
			},
			{
				// The step under test: iam_role S3 -> azure_blob, with a known
				// external_id in state and null in the result.
				Config: logExportAzureFakeConfig("barndoor/"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						// Pins WHY the apply below is consistent, rather than
						// leaving it to the absence of an error. Because
						// external_id is Computed and null in configuration, the
						// framework marks it unknown on any plan whose proposed
						// state differs from prior state (fwserver's
						// MarkComputedNilsAsUnknown, which keys on the CONFIG
						// value being null — not the planned value). So a
						// provider/auth switch already plans "(known after
						// apply)" and mapping it to null is consistent. If that
						// ever stops holding, this fails here with a clear
						// reason instead of surfacing as "Provider produced
						// inconsistent result after apply" for a practitioner.
						plancheck.ExpectUnknownValue(
							logExportResourceName,
							tfjsonpath.New("destination").AtMapKey("external_id"),
						),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(logExportResourceName, "destination.provider", storageProviderAzureBlob),
					resource.TestCheckResourceAttr(logExportResourceName, "destination.auth_method", authMethodAccountKey),
					resource.TestCheckResourceAttr(logExportResourceName, "destination.path_prefix", "barndoor/"),
					// The S3-only attributes settle on their schema defaults,
					// not on the empty values the API reports for an Azure row.
					resource.TestCheckResourceAttr(logExportResourceName, "destination.use_ssl", "true"),
					resource.TestCheckResourceAttr(logExportResourceName, "destination.use_path_style", "false"),
					resource.TestCheckNoResourceAttr(logExportResourceName, "destination.external_id"),
					resource.TestCheckNoResourceAttr(logExportResourceName, "destination.region"),
					resource.TestCheckNoResourceAttr(logExportResourceName, "destination.iam_role_arn"),
					checkAzurePutBodyOmitsS3Fields(fake),
				),
			},
			{
				// A re-plan over the Azure destination must be empty.
				Config:   logExportAzureFakeConfig("barndoor/"),
				PlanOnly: true,
			},
			{
				// And back again, exercising the reverse transition (null
				// external_id in state, known value in the result).
				Config: logExportIAMRoleConfig(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(logExportResourceName, "destination.external_id", fakeExportExternalID),
				),
			},
			{
				Config:   logExportIAMRoleConfig(),
				PlanOnly: true,
			},
		},
	})
}

// TestLogExportResource_iamRoleToAccessKeysLifecycle covers the same transition
// class within S3: external_id is minted for iam_role and null for access_keys.
// This direction predates the Azure work, so it also pins pre-existing
// behaviour that was never covered by a real plan/apply.
func TestLogExportResource_iamRoleToAccessKeysLifecycle(t *testing.T) {
	setupExportTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: logExportIAMRoleConfig(),
				Check: resource.TestCheckResourceAttr(
					logExportResourceName, "destination.external_id", fakeExportExternalID),
			},
			{
				// iam_role -> access_keys: known external_id in state, null
				// after apply.
				Config: logExportAccessKeysConfig(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(logExportResourceName, "destination.auth_method", authMethodAccessKeys),
					resource.TestCheckResourceAttr(logExportResourceName, "destination.has_credentials", "true"),
					resource.TestCheckNoResourceAttr(logExportResourceName, "destination.external_id"),
					resource.TestCheckNoResourceAttr(logExportResourceName, "destination.iam_role_arn"),
				),
			},
			{
				Config:   logExportAccessKeysConfig(),
				PlanOnly: true,
			},
		},
	})
}

// TestLogExportResource_azureSASLifecycle exercises the sas_token auth method
// and an auth-method switch within azure_blob (account_key -> sas_token), which
// the API requires a fresh secret for.
func TestLogExportResource_azureSASLifecycle(t *testing.T) {
	fake := setupExportTest(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: logExportAzureFakeConfig("barndoor/"),
				Check: resource.TestCheckResourceAttr(
					logExportResourceName, "destination.auth_method", authMethodAccountKey),
			},
			{
				Config: logExportAzureSASFakeConfig(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(logExportResourceName, "destination.auth_method", authMethodSASToken),
					resource.TestCheckResourceAttr(logExportResourceName, "destination.use_ssl", "true"),
					resource.TestCheckNoResourceAttr(logExportResourceName, "destination.path_prefix"),
					// The API strips a leading '?' from the SAS token; the
					// attribute is config-only, so state keeps what was written
					// and the normalization causes no drift.
					resource.TestCheckResourceAttr(logExportResourceName, "destination.sas_token", "?sv=2024-01-01&sig=abc"),
					checkStoredSASToken(fake, "sv=2024-01-01&sig=abc"),
				),
			},
			{
				Config:   logExportAzureSASFakeConfig(),
				PlanOnly: true,
			},
		},
	})
}

// checkAzurePutBodyOmitsS3Fields asserts on the bytes the provider actually
// sent. The fake already 400s on a stray `use_ssl: true`, but this reports the
// offending key by name instead of a generic apply failure.
func checkAzurePutBodyOmitsS3Fields(fake *fakeExportServer) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		body := fake.lastRecordedPutBody()
		if body == nil {
			return fmt.Errorf("no destination PUT was recorded")
		}
		for _, forbidden := range []string{
			"region", "use_ssl", "use_path_style", "iam_role_arn", "access_key_id", "secret_access_key",
		} {
			if _, present := body[forbidden]; present {
				raw, _ := json.Marshal(body)
				return fmt.Errorf("azure destination PUT carried %q, which the API rejects; body = %s", forbidden, raw)
			}
		}
		if _, present := body["account_key"]; !present {
			return fmt.Errorf("azure destination PUT did not carry account_key")
		}
		return nil
	}
}

func checkStoredSASToken(fake *fakeExportServer, want string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		dest := fake.currentDestination()
		if dest == nil {
			return fmt.Errorf("no destination stored")
		}
		if dest.Secret != want {
			return fmt.Errorf("stored sas_token = %q, want %q", dest.Secret, want)
		}
		return nil
	}
}

func logExportIAMRoleConfig() string {
	return `
resource "barndoor_log_export" "test" {
  destination = {
    endpoint     = "https://s3.us-east-1.amazonaws.com"
    region       = "us-east-1"
    bucket       = "audit-logs"
    auth_method  = "iam_role"
    iam_role_arn = "arn:aws:iam::123456789012:role/barndoor"
  }

  enabled = false
}
`
}

func logExportAccessKeysConfig() string {
	return `
resource "barndoor_log_export" "test" {
  destination = {
    endpoint          = "https://s3.us-east-1.amazonaws.com"
    region            = "us-east-1"
    bucket            = "audit-logs"
    auth_method       = "access_keys"
    access_key_id     = "AKIAIOSFODNN7EXAMPLE"
    secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
  }

  enabled = false
}
`
}

func logExportAzureFakeConfig(pathPrefix string) string {
	return fmt.Sprintf(`
resource "barndoor_log_export" "test" {
  destination = {
    provider    = "azure_blob"
    endpoint    = "https://acmeaudit.blob.core.windows.net"
    bucket      = "audit-logs"
    path_prefix = %q
    auth_method = "account_key"
    account_key = "azure-account-key"
  }

  enabled = false
}
`, pathPrefix)
}

func logExportAzureSASFakeConfig() string {
	return `
resource "barndoor_log_export" "test" {
  destination = {
    provider    = "azure_blob"
    endpoint    = "https://acmeaudit.blob.core.windows.net"
    bucket      = "audit-logs"
    auth_method = "sas_token"
    sas_token   = "?sv=2024-01-01&sig=abc"
  }

  enabled = false
}
`
}
