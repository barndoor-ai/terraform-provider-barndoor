# Changelog

## 0.1.0 (2026-07-01)

FEATURES:

* **New Resource:** `barndoor_log_export` — manages an organization's audit-log export: the customer-owned S3-compatible destination, delivery settings, and whether streaming is enabled.
* **New Data Source:** `barndoor_log_export_aws_trust_info` — reads Barndoor's AWS principal ARN and the per-destination external ID so a customer can build the `aws_iam_role` trust policy for the export's `iam_role` auth method in one `terraform apply`.

ENHANCEMENTS:

* provider: API error diagnostics now bound the response body shown in `terraform plan`/`apply` output and, when the body is a JSON object, surface its `message`/`error`/`detail` field instead of dumping the whole object. The full response body is logged at `DEBUG` level for troubleshooting.
