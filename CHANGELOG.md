# Changelog

## 0.3.0 (Unreleased)

FEATURES:

* **New Resource:** `barndoor_connection` — manages the organization's tenant-wide (service-account-owned) credential connection to an MCP server, for non-OAuth credential providers (`api_key`, `bearer_token`, `basic_auth`, `generic`). Credentials are write-only and any change forces a new connection. OAuth providers are rejected with an explanatory error — their interactive browser consent cannot be performed by a declarative apply. Requires platform support for `as_service` on the connection read endpoint (bdai-platform BCP-3256).
* **New Data Source:** `barndoor_policy` — looks up an existing access policy by `id` or `name` (exact match among non-archived policies) and exposes its full attribute set, including rules.
* **New Data Source:** `barndoor_agent` — looks up an existing AI Agent registration by `id` or display `name`; ambiguous display names fail loudly with the candidate ids.
* **New Data Source:** `barndoor_mcp_server` — looks up an existing MCP server by `id`, `name` (matched case- and whitespace-insensitively, mirroring the API's uniqueness rule), or `slug`. Credential attributes are never part of the data source.
* **New Resource:** `barndoor_dlp_org_config` — manages the organization's singleton Data Protection configuration (`enabled`, `global_dry_run`) over the dlp-service tenant admin REST API. The platform provisions the row per organization, so the resource adopts and configures it; `terraform destroy` resets both settings to the platform defaults (`enabled = true`, `global_dry_run = false`) rather than deleting anything (BCP-3257).
* **New Resource:** `barndoor_dlp_enforcement_policy` — manages a Data Protection enforcement policy: MCP-server or model-provider targeting (with API-side `target_kind` inference), runtime stage, action, priority (API-assigned when unset), dry-run flag, principal scoping, and the detection engines that evaluate the traffic (BCP-3257).
* **New Resource:** `barndoor_dlp_allow_list_entry` — manages one Data Protection allow-list entry (literal or regex pattern, optional detection-type scoping, audit reason). The platform API has no update endpoint and no get-by-id, so every attribute change replaces the entry and reads walk the paginated list (BCP-3257).

## 0.2.0 (2026-07-02)

BREAKING CHANGES:

* provider: `base_url` is now the Barndoor platform **host root** (e.g. `https://platform.barndoor.ai`) instead of the system-management public API URL; the provider appends each service's API prefix itself. Configuration fails with an explicit migration error when `base_url` still carries a path. Migration: drop the `/api/system-management/public/v1` suffix from `base_url` / `BARNDOOR_BASE_URL`.

FEATURES:

* **New Resource:** `barndoor_policy` — manages an MCP-server access policy over the `barndoor.policy.v2` gRPC contract: AI Agent bindings, tags, lifecycle status (`DRAFT`/`ACTIVE`/`INACTIVE`), and rules with effects, actions, roles, and JSON condition trees. `terraform destroy` archives the policy (the platform's terminal lifecycle state).
* **New Resource:** `barndoor_mcp_server` — manages an MCP server instance over the registry public REST API: the directory entry it instantiates, tenant OAuth or pre-populated credentials (write-only), and scope overrides. `terraform destroy` soft-deletes the server (the platform tears down its connections and stored credentials).
* **New Resource:** `barndoor_agent` — manages an AI Agent registration over the registry public REST API: binds an agent directory entry to the organization (attaching its machine-to-machine service account) and manages the per-agent `write_confirmations_required` / `llm_gateway_enabled` toggles. `terraform destroy` unregisters the agent; the platform archives dependent policies.

ENHANCEMENTS:

* provider: gRPC transport — the provider now maintains a lazily-created, shared gRPC channel to the platform host (TLS with system roots on port 443 unless `base_url` carries an explicit port) with per-RPC bearer-token credentials minted from the same `client_credentials` grant used for REST.

BUG FIXES:

* data-source/barndoor_log_export_aws_trust_info: the published example used HCL block syntax (`destination { … }`) for the `destination` attribute of `barndoor_log_export`, which is a nested attribute and requires assignment syntax (`destination = { … }`); copying the example produced invalid configuration. The acceptance-test HCL had the same bug.

## 0.1.0 (2026-07-01)

FEATURES:

* **New Resource:** `barndoor_log_export` — manages an organization's audit-log export: the customer-owned S3-compatible destination, delivery settings, and whether streaming is enabled.
* **New Data Source:** `barndoor_log_export_aws_trust_info` — reads Barndoor's AWS principal ARN and the per-destination external ID so a customer can build the `aws_iam_role` trust policy for the export's `iam_role` auth method in one `terraform apply`.

ENHANCEMENTS:

* provider: API error diagnostics now bound the response body shown in `terraform plan`/`apply` output and, when the body is a JSON object, surface its `message`/`error`/`detail` field instead of dumping the whole object. The full response body is logged at `DEBUG` level for troubleshooting.
