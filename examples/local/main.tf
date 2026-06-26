# Minimal config for testing the provider against a LOCAL Barndoor platform
# (`make tilt`). See ../../CONTRIBUTING.md for the full loop.
#
# Run with the locally-built provider via dev_overrides (NO `terraform init`):
#   make dev-install                 # in the repo root; prints the ~/.terraformrc block
#   export BARNDOOR_CLIENT_ID=...    # see CONTRIBUTING.md section 3
#   export BARNDOOR_CLIENT_SECRET=...
#   export BARNDOOR_ORGANIZATION_ID=...
#   terraform plan
#   terraform apply
#
# Or keep dev_overrides scoped to this dir instead of editing ~/.terraformrc:
#   TF_CLI_CONFIG_FILE=$PWD/dev.tfrc terraform plan

terraform {
  required_providers {
    barndoor = {
      source = "barndoor-ai/barndoor"
    }
  }
}

# Connection identifiers are non-secret and pinned for local dev. The credential
# (BARNDOOR_CLIENT_ID / BARNDOOR_CLIENT_SECRET) and org (BARNDOOR_ORGANIZATION_ID)
# are read from the environment by the provider — keep secrets out of config.
provider "barndoor" {
  base_url  = "https://mcp.barndoorlocal.com/api/system-management/public/v1"
  token_url = "https://auth.barndoorlocal.com/realms/barndoor/protocol/openid-connect/token"
}

# Configures the datadog-json export that the platform auto-provisions for every
# org. enabled = false means the API stores the destination but runs NO S3
# connectivity probe (that only happens on start), so the dummy credentials below
# never touch S3 — a safe smoke test without a real bucket.
resource "barndoor_log_export" "audit" {
  enabled = false

  destination = {
    endpoint          = "https://s3.us-east-1.amazonaws.com"
    region            = "us-east-1"
    bucket            = "barndoor-local-test"
    access_key_id     = "AKIALOCALTESTONLY"
    secret_access_key = "local-test-secret-not-real"
  }
}

output "export_enabled" {
  value = barndoor_log_export.audit.enabled
}

output "export_has_credentials" {
  description = "Server-confirmed that the destination credentials were stored."
  value       = barndoor_log_export.audit.destination.has_credentials
}
