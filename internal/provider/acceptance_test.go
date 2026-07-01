// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MIT

package provider

// Acceptance tests for the Barndoor provider.
//
// # Running
//
// These tests are gated on TF_ACC (the standard Terraform acceptance-test
// switch) AND the BARNDOOR_* connection environment variables. With either
// unset they skip cleanly, so `go test ./...` and the CI test job (which sets
// TF_ACC=1 but not the credentials) never reach a live backend.
//
// To run them, point the provider at a reachable Barndoor environment and a
// service-account credential scoped to the organization under test:
//
//	export TF_ACC=1
//	export BARNDOOR_BASE_URL=https://platform.barndoor.ai/api/system-management/public/v1
//	export BARNDOOR_TOKEN_URL=https://auth.barndoor.ai/realms/barndoor/protocol/openid-connect/token
//	export BARNDOOR_CLIENT_ID=...            # a client_credentials client
//	export BARNDOOR_CLIENT_SECRET=...        # the client's secret
//	export BARNDOOR_ORGANIZATION_ID=...      # the org the credential is scoped to
//	go test ./internal/provider/ -run TestAcc -v
//
// # Safety: do not clobber a real export
//
// An organization's audit-log export may be actively delivering. The write
// tests must never create, modify, or delete the export configuration of an
// organization you care about.
//
// Therefore:
//
//   - TestAccConnectivity is READ-ONLY (a single GET) and safe to run against
//     any org: it proves a client_credentials token mints and the /public/v1
//     read path is reachable and authorized.
//
//   - The write tests (TestAccLogExportResource_lifecycle and
//     TestAccLogExportAWSTrustInfoDataSource — the latter mints/persists an
//     external ID) only run when BARNDOOR_ACC_TEST_ORGANIZATION_ID names a
//     DISPOSABLE test org whose export configuration may be freely changed.
//     They skip with an explicit reason otherwise. As an extra guard, set
//     BARNDOOR_ACC_PROTECTED_ORGANIZATION_ID to an organization that must never
//     be touched (e.g. a production org) and the write tests hard-fail if the
//     disposable-org variable is ever pointed at it.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/barndoor-ai/terraform-provider-barndoor/internal/client"
)

const (
	// envTestOrgID opts in to the destructive (write) acceptance tests by
	// naming a disposable org whose export config may be freely changed.
	envTestOrgID = "BARNDOOR_ACC_TEST_ORGANIZATION_ID"

	// envProtectedOrgID optionally names an organization that must never be used
	// for a destructive test (e.g. a production org). When set, the write tests
	// hard-fail if envTestOrgID is pointed at it.
	envProtectedOrgID = "BARNDOOR_ACC_PROTECTED_ORGANIZATION_ID"
)

// connEnv lists the connection variables every acceptance test needs.
var connEnv = []string{
	"BARNDOOR_BASE_URL",
	"BARNDOOR_TOKEN_URL",
	"BARNDOOR_CLIENT_ID",
	"BARNDOOR_CLIENT_SECRET",
	"BARNDOOR_ORGANIZATION_ID",
}

// testAccProtoV6ProviderFactories builds the in-process provider server the
// terraform-plugin-testing harness drives. The provider reads its connection
// settings from the BARNDOOR_* environment variables.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"barndoor": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccPreCheck skips the test unless the full BARNDOOR_* connection
// environment is present. (resource.TestCase additionally requires TF_ACC.)
func testAccPreCheck(t *testing.T) {
	t.Helper()
	for _, env := range connEnv {
		if os.Getenv(env) == "" {
			t.Skipf("%s not set; skipping acceptance test (set TF_ACC and the BARNDOOR_* connection env to run)", env)
		}
	}
}

// requireDisposableTestOrg returns the disposable test org for a destructive
// test, or skips. If BARNDOOR_ACC_PROTECTED_ORGANIZATION_ID is set, it hard-fails
// when the opt-in variable is pointed at that org, so a misconfiguration can
// never clobber an organization you have marked off-limits.
func requireDisposableTestOrg(t *testing.T) string {
	t.Helper()
	org := os.Getenv(envTestOrgID)
	if org == "" {
		t.Skipf("%s not set: refusing to run a destructive (write) acceptance test, which would clobber a "+
			"real export configuration. Set it to a DISPOSABLE test org (whose export config may be freely "+
			"changed) backed by a credential scoped to that org. See the file header in acceptance_test.go for details.", envTestOrgID)
	}
	if protected := os.Getenv(envProtectedOrgID); protected != "" && org == protected {
		t.Fatalf("%s is set to the protected org named by %s, which must never be used for a destructive "+
			"acceptance test. Refusing to run.", envTestOrgID, envProtectedOrgID)
	}
	return org
}

// TestAccConnectivity is a read-only smoke test: it mints a client_credentials
// token and reads the configured org's export over the /public/v1 path. It
// never mutates anything, so it is safe to run against any org, and it proves
// the auth + read path end to end (token mint + authorized read).
func TestAccConnectivity(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set; skipping acceptance test")
	}
	testAccPreCheck(t)

	c := client.New(client.Config{
		BaseURL:        os.Getenv("BARNDOOR_BASE_URL"),
		TokenURL:       os.Getenv("BARNDOOR_TOKEN_URL"),
		ClientID:       os.Getenv("BARNDOOR_CLIENT_ID"),
		ClientSecret:   os.Getenv("BARNDOOR_CLIENT_SECRET"),
		OrganizationID: os.Getenv("BARNDOOR_ORGANIZATION_ID"),
	})

	// GET .../exports/{org}/{exportType} is purely read-only. Calling Do mints
	// the token, so a successful (or authorized-but-404) response proves the
	// whole auth + edge read path end to end.
	path := exportPath(c.OrganizationID(), defaultExportType)
	resp, err := c.Do(context.Background(), http.MethodGet, path, nil)
	if err != nil {
		t.Fatalf("connectivity check failed (token mint or request): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Token minted and the authorized edge read worked; the org just has no
		// export row provisioned. Connectivity is still verified.
		t.Logf("connectivity OK: authorized, but no %q export provisioned for org %s", defaultExportType, c.OrganizationID())
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		t.Logf("connectivity OK: authorized read of %q export for org %s", defaultExportType, c.OrganizationID())
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		t.Fatalf("connectivity check failed: GET %s -> %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// TestAccLogExportResource_lifecycle exercises create → update → import →
// destroy against a disposable dev org. Destructive: gated on a disposable test
// org (see requireDisposableTestOrg).
func TestAccLogExportResource_lifecycle(t *testing.T) {
	testOrg := requireDisposableTestOrg(t)
	const resourceName = "barndoor_log_export.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Create: configure a destination + settings, streaming disabled
				// (enabled=true would trigger the server's connectivity probe
				// against the bucket, which a placeholder destination fails).
				Config: testAccLogExportConfig(testOrg, 100),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "organization_id", testOrg),
					resource.TestCheckResourceAttr(resourceName, "export_type", defaultExportType),
					resource.TestCheckResourceAttr(resourceName, "enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "destination.bucket", "barndoor-acc-test-disposable"),
					resource.TestCheckResourceAttr(resourceName, "destination.auth_method", authMethodAccessKeys),
					resource.TestCheckResourceAttr(resourceName, "destination.has_credentials", "true"),
					resource.TestCheckResourceAttr(resourceName, "settings.batch_size", "100"),
				),
			},
			{
				// Update: change a setting in place.
				Config: testAccLogExportConfig(testOrg, 250),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "settings.batch_size", "250"),
				),
			},
			{
				// Import: secrets are config-only (never returned by the API), so
				// they read back null on import and are excluded from the diff.
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateId:     fmt.Sprintf("%s/%s", testOrg, defaultExportType),
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					"destination.access_key_id",
					"destination.secret_access_key",
				},
			},
		},
	})
}

// TestAccLogExportAWSTrustInfoDataSource reads the trust-info data source.
// Destructive-ish: the read mints and persists an external ID on the
// destination, and requires the iam_role feature to be enabled for the org, so
// it is gated on a disposable test org. (Lives here so the acceptance suite is
// in one place; the data source itself ships in the trust-info PR.)
func TestAccLogExportAWSTrustInfoDataSource(t *testing.T) {
	testOrg := requireDisposableTestOrg(t)
	const dataName = "data.barndoor_log_export_aws_trust_info.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccTrustInfoConfig(testOrg),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(dataName, "organization_id", testOrg),
					resource.TestCheckResourceAttr(dataName, "export_type", defaultExportType),
					resource.TestCheckResourceAttrSet(dataName, "principal_arn"),
					resource.TestCheckResourceAttrSet(dataName, "external_id"),
				),
			},
		},
	})
}

// testAccLogExportConfig renders a log-export resource for the disposable org.
// The provider self-configures from the BARNDOOR_* environment. The S3 values
// are placeholders (AWS's documented example access key) — streaming stays
// disabled so they are stored but never probed.
func testAccLogExportConfig(orgID string, batchSize int) string {
	return fmt.Sprintf(`
resource "barndoor_log_export" "test" {
  organization_id = %[1]q

  destination {
    endpoint          = "https://s3.us-east-1.amazonaws.com"
    region            = "us-east-1"
    bucket            = "barndoor-acc-test-disposable"
    auth_method       = "access_keys"
    access_key_id     = "AKIAIOSFODNN7EXAMPLE"
    secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
  }

  settings {
    batch_size = %[2]d
  }

  enabled = false
}
`, orgID, batchSize)
}

func testAccTrustInfoConfig(orgID string) string {
	return fmt.Sprintf(`
data "barndoor_log_export_aws_trust_info" "test" {
  organization_id = %[1]q
}
`, orgID)
}
