// Copyright (c) Barndoor AI, Inc.
// SPDX-License-Identifier: MPL-2.0

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
// To run them, point the provider at a reachable environment (dev is
// Tailscale-only):
//
//	export TF_ACC=1
//	export BARNDOOR_BASE_URL=https://platform.barndoordev.com/api/system-management/public/v1
//	export BARNDOOR_TOKEN_URL=https://auth.barndoordev.com/realms/barndoor/protocol/openid-connect/token
//	export BARNDOOR_CLIENT_ID=...            # a client_credentials client
//	export BARNDOOR_CLIENT_SECRET=...        # from the Keycloak admin UI
//	export BARNDOOR_ORGANIZATION_ID=...      # the org the credential is scoped to
//	go test ./internal/provider/ -run TestAcc -v
//
// # Safety: do not clobber a live export
//
// The dev bootstrap credential (barndoor-log-export-bootstrap) is scoped to the
// platform setup org, which has a LIVE, actively-delivering export. Its export
// configuration must never be created, modified, or deleted by a test.
//
// Therefore:
//
//   - TestAccConnectivity is READ-ONLY (a single GET) and safe to run against
//     any org, including the live setup org. It is the formalized Phase 0.2
//     verification: prove a client_credentials token mints and the /public/v1
//     edge read path is reachable and authorized.
//
//   - The write tests (TestAccLogExportResource_lifecycle and
//     TestAccLogExportAWSTrustInfoDataSource — the latter mints/persists an
//     external ID) only run when BARNDOOR_ACC_TEST_ORGANIZATION_ID names a
//     DISPOSABLE dev test org. They skip with an explicit reason otherwise, and
//     hard-refuse (fail) if that variable is ever set to the known live setup
//     org. Run them only with a credential scoped to a throwaway org whose
//     export row may be freely reconfigured.

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
	// liveSetupOrgID is the platform setup org used to bootstrap dev auth
	// (Phase 0.1/0.2). It hosts a live export; destructive tests refuse it.
	liveSetupOrgID = "fcdc562c-546c-4cca-8fee-e557a642dc9d"

	// envTestOrgID opts in to the destructive (write) acceptance tests by
	// naming a disposable dev org whose export config may be clobbered.
	envTestOrgID = "BARNDOOR_ACC_TEST_ORGANIZATION_ID"
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
// test, or skips. It hard-fails if the opt-in variable is ever pointed at the
// known live setup org, so a misconfiguration can never clobber the real export.
func requireDisposableTestOrg(t *testing.T) string {
	t.Helper()
	org := os.Getenv(envTestOrgID)
	if org == "" {
		t.Skipf("%s not set: refusing to run a destructive (write) acceptance test, which would clobber a "+
			"real export configuration. Set it to a DISPOSABLE dev test org (never the live setup org) backed "+
			"by a credential scoped to that org. See the file header in acceptance_test.go for details.", envTestOrgID)
	}
	if org == liveSetupOrgID {
		t.Fatalf("%s is set to the live setup org %s, which has an actively-delivering export. Refusing to run a "+
			"destructive acceptance test against it.", envTestOrgID, liveSetupOrgID)
	}
	return org
}

// TestAccConnectivity is a read-only smoke test: it mints a client_credentials
// token and reads the configured org's export over the /public/v1 edge path.
// It never mutates anything, so it is safe against the live setup org, and it
// formalizes the Phase 0.2 verification (token mint + authorized edge read).
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
