# Read Barndoor's AWS principal ARN and the per-destination external ID so the
# customer's IAM role trust policy can be built in the same `terraform apply` as
# the export itself — no copy-paste of values from a console.
#
# Requires the `iam_role` auth method to be enabled for the organization.
data "barndoor_log_export_aws_trust_info" "audit" {
  # organization_id defaults to the provider's organization_id.
  # export_type defaults to "datadog-json".
}

# Grant Barndoor's principal sts:AssumeRole, locked to the external ID so only
# Barndoor's calls for this destination can assume the role.
resource "aws_iam_role" "barndoor_export" {
  name = "barndoor-log-export"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { AWS = data.barndoor_log_export_aws_trust_info.audit.principal_arn }
      Condition = {
        StringEquals = {
          "sts:ExternalId" = data.barndoor_log_export_aws_trust_info.audit.external_id
        }
      }
    }]
  })
}

# Feed the role ARN straight back into the export destination — one apply, end to
# end, with no long-lived S3 keys and no secrets in Terraform state.
resource "barndoor_log_export" "audit" {
  destination {
    endpoint     = "https://s3.us-east-1.amazonaws.com"
    region       = "us-east-1"
    bucket       = "acme-barndoor-audit"
    auth_method  = "iam_role"
    iam_role_arn = aws_iam_role.barndoor_export.arn
  }

  enabled = true
}
