# Changelog

## 0.2.0 (Unreleased)

BREAKING CHANGES:

* provider: `base_url` is now the Barndoor platform **host root** (e.g. `https://platform.barndoor.ai`) instead of the system-management public API URL; the provider appends each service's API prefix itself. Configuration fails with an explicit migration error when `base_url` still carries a path. Migration: drop the `/api/system-management/public/v1` suffix from `base_url` / `BARNDOOR_BASE_URL`.

FEATURES:

* **New Resource:** `barndoor_policy` — manages an MCP-server access policy over the `barndoor.policy.v2` gRPC contract: AI Agent bindings, tags, lifecycle status (`DRAFT`/`ACTIVE`/`INACTIVE`), and rules with effects, actions, roles, and JSON condition trees. `terraform destroy` archives the policy (the platform's terminal lifecycle state).

ENHANCEMENTS:

* provider: gRPC transport — the provider now maintains a lazily-created, shared gRPC channel to the platform host (TLS with system roots on port 443 unless `base_url` carries an explicit port) with per-RPC bearer-token credentials minted from the same `client_credentials` grant used for REST.

## 0.1.0 (2026-07-01)

FEATURES:

* **New Resource:** `barndoor_log_export` — manages an organization's audit-log export: the customer-owned S3-compatible destination, delivery settings, and whether streaming is enabled.
* **New Data Source:** `barndoor_log_export_aws_trust_info` — reads Barndoor's AWS principal ARN and the per-destination external ID so a customer can build the `aws_iam_role` trust policy for the export's `iam_role` auth method in one `terraform apply`.

ENHANCEMENTS:

* provider: API error diagnostics now bound the response body shown in `terraform plan`/`apply` output and, when the body is a JSON object, surface its `message`/`error`/`detail` field instead of dumping the whole object. The full response body is logged at `DEBUG` level for troubleshooting.
