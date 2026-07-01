# Stream an organization's audit logs to a customer-owned S3 bucket using
# access-key credentials. organization_id and export_type default to the
# provider's organization and "datadog-json" respectively.
resource "barndoor_log_export" "audit" {
  enabled = true

  destination = {
    endpoint          = "https://s3.us-east-1.amazonaws.com"
    region            = "us-east-1"
    bucket            = "acme-barndoor-audit-logs"
    path_prefix       = "barndoor/"
    auth_method       = "access_keys"
    access_key_id     = var.s3_access_key_id
    secret_access_key = var.s3_secret_access_key
  }

  settings = {
    batch_size             = 100
    flush_interval_seconds = 30
    max_retries            = 3
    included_event_types   = ["audit.policy.updated", "audit.user.login"]
  }
}

# Alternatively, let Barndoor assume an IAM role you control. After the first
# apply, read barndoor_log_export.audit_iam.destination.external_id to build the
# role's trust policy (sts:ExternalId). Requires the iam_role feature for the org.
resource "barndoor_log_export" "audit_iam" {
  export_type = "datadog-json"
  enabled     = false

  destination = {
    endpoint     = "https://s3.us-east-1.amazonaws.com"
    region       = "us-east-1"
    bucket       = "acme-barndoor-audit-logs"
    auth_method  = "iam_role"
    iam_role_arn = "arn:aws:iam::123456789012:role/barndoor-log-export"
  }
}
