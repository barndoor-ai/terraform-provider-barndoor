# Changelog

## Unreleased

FEATURES:

* **New Resource:** `barndoor_llm_connection` manages an LLM Gateway connection: a named, shareable credential for a model vendor that providers reference with `connection_id`. It is now the only place an upstream secret can live, because the platform stopped accepting keys on providers in v2.40.0 (BCP-3647).
  * `api_key` (API-key auth types) and `credentials` (structured secrets for `aws_static_credentials`, `azure_entra_client_secret` or `google_service_account`) are sensitive and write-only. Terraform keeps the configured value, re-sends it on every update, and reports only `key_last4`.
  * `settings` holds the resource settings, such as a Bedrock `region` and `iam_role_arn`. The platform adds derived keys on write, and those produce no diff. `effective_settings` shows the stored object, including the generated `external_id` an `aws_role` trust policy must require.
  * Changing `base_url` moves every provider that follows the connection's endpoint.
  * Destroying a connection that a provider managed outside this configuration still uses fails with the platform's 409.
  * Import is by id.

BUG FIXES:

* resource/`barndoor_llm_provider`: since platform v2.40.0, creating any provider whose auth type stores a secret has failed, as has any update that re-sent `api_key`. The platform now rejects inline keys and requires `connection_id`, and the resource had no way to supply one. It now takes a `connection_id` (see ENHANCEMENTS), and `api_key` is deprecated.
* resource/`barndoor_llm_provider`, resource/`barndoor_llm_connection`: a `base_url` ending in `/v1` for the OpenAI-compatible families (`openai`, `anthropic`, `groq`, `together`, `mistral`, `cohere`, `xai`, `fireworks`, `perplexity`, `openrouter`, `deepseek`, `custom`) now fails at plan time, with the corrected URL. The platform rejects it because the gateway appends the version itself. The previous documentation example `https://api.openai.com/v1` was one of these URLs.

* resource/`barndoor_llm_governance_config`: every apply, and `terraform destroy`, reset settings the resource did not manage to their platform defaults. The platform's update replaces the whole configuration row, and the resource sent only `require_pricing_for_mappings`. As a result, an organization switched to `default_model_access = "deny"` in the app went back to `allow`, reopening model access to every model, and `require_routing_policy` was turned off. The resource now reads the current configuration and changes only what it manages, and destroy resets only `require_pricing_for_mappings`.
* resource/`barndoor_llm_model_mapping`: `stream_idle_timeout_secs` accepted only 1–120, but the platform allows 1–300 and writes 180 by default. A mapping created with the platform default therefore held a value its own configuration could not express. The validator now matches the platform.
* resource/`barndoor_llm_provider`: `model_provider` now accepts `azure_foundry`, which the platform supports, and the `auth_type` documentation lists the real per-provider defaults (`bedrock` → `aws_role`, `vertex` → `google_adc`, `azure_foundry` → `azure_foundry_api_key`).

ENHANCEMENTS:

* resource/`barndoor_llm_provider`, data-source/`barndoor_llm_provider`: new `connection_id` attribute, the `barndoor_llm_connection` the provider reads its upstream secret from.
  * The connection supplies the provider's `auth_type`, its resource settings and, when the provider's `base_url` is unset (now optional), its endpoint. Setting `auth_type` or `api_key` together with `connection_id` is a plan-time error.
  * Changing `connection_id` rebinds the provider in place. Removing it forces replacement, because the platform cannot detach a provider from its credential.
  * Providers with a request-scoped OAuth passthrough (`claude_oauth`, `codex_oauth`) still need no connection.
  * `settings` no longer has to be written in the platform's normalized form. While every configured key keeps its value, keys the platform merges in or derives produce no diff.

* resource/`barndoor_llm_governance_config`: new optional `default_model_access` (`allow` or `deny`) and `require_routing_policy` attributes. Each keeps its stored value when unset. Setting `deny` requires an enabled allowlist, and the API rejects it otherwise. Neither is changed by destroy, so removing the resource never loosens model access. Requires platform support for the deny posture (bdai-platform BCP-3887) and routing-policy enforcement (BCP-4037).

* resource/`barndoor_llm_token_budget`, resource/`barndoor_llm_rate_limit`: new optional `member_of_group` attribute, which narrows a broad per-user rule to one IdP group's members. Each member gets their **own** allowance from the single rule, and someone who joins the group picks it up on their next request. That is the opposite of `scope_type = "group"`, which is **one** allowance pooled across the group. Only valid with `scope_type = "user"` and neither `scope_id` nor `scope_value` set. Those shapes are now rejected at plan time instead of at apply. Neither update API accepts the field, so changing or removing it forces replacement. Before this change, a filtered rule could be managed only from the Barndoor app. Requires platform support for `member_of_group` (bdai-platform BCP-3814 / #7195) (BCP-3897).
* resource/`barndoor_llm_model_mapping`: six new optional attributes set the route's passive cooldown policy. That policy decides when the gateway stops sending traffic to a failing route and fails over to the next one. `cooldown_failure_threshold` (0–100, default 10; `0` disables the route's cooldowns) and `cooldown_window_secs` (1–3600, default 60) set when a route cools down. `cooldown_base_secs` (1–3600, default 30; doubled on each failed recovery probe), `cooldown_max_secs` (1–86400, default 300; caps every cooldown), `cooldown_429_default_secs` (1–3600, default 30) and `cooldown_overloaded_secs` (0–3600, default 10; `0` treats a 529 as an ordinary failure) set how long it lasts. Unset attributes take the platform default on create and keep the stored value afterwards. Base, 429 and non-zero 529 cooldowns above `cooldown_max_secs` now fail at plan time. Before this change, replacing a mapping silently reset a policy tuned in the app to the defaults. Requires platform v2.43.0 or later (bdai-platform BCP-2671 / #8119) (BCP-4488).
* resource/`barndoor_llm_provider`, data-source/`barndoor_llm_provider`: new `billing_mode` (`per_token`, the default, or `not_metered`), `billing_reason` (`subscription`, `local`, `external`, `other`) and `billing_note` (200 characters at most) attributes. A `not_metered` provider still reports token usage, but records its token cost as $0. It requires a `billing_reason`, and that rule is now checked at plan time. A reason is also allowed on `per_token`, for a subscription that bills overages per token: the two attributes are independent. The resource leaves these attributes alone unless they are configured, so a configuration that never mentions billing does not disturb billing set in the app. Removing `billing_reason` or `billing_note` from a configuration that set it clears it. Removing `billing_mode` keeps the stored mode. Changing `billing_mode` is not retroactive: usage already recorded keeps its cost. Requires platform support for provider billing attributes (bdai-platform BCP-3876 / #7301) (BCP-3949).

## 0.7.0 (2026-09-21)

ENHANCEMENTS:

* resource/`barndoor_mcp_server`, data-source/`barndoor_mcp_server`: new read-only `attention_tier` attribute — what the server needs from an administrator next, as a single value (`pending_publish`, `connection_error`, `pending_credentials`, `pending_connection`, or `available`). It is computed per read rather than stored, and resolved against the organization's `mcp-server-publishing` feature flag: with that flag off, a server that is otherwise ready but unpublished reads `available` rather than `pending_publish`. Treat it as a reporting signal, not a publish precondition — `publish_blockers` is the attribute to assert on for that. Requires platform support for `attention_tier` on the server response (bdai-platform BCP-3815) (BCP-4012).
* resource/`barndoor_mcp_server`, data-source/`barndoor_mcp_server`: new read-only `publish_blockers` attribute — why a publish would be rejected right now, listed in the order the registry's publish gate evaluates them (`not_operationally_available` and/or `no_active_policy`, the same two preconditions `barndoor_mcp_server_publication` retries on during create). Empty and null mean different things and both round-trip: `[]` is "the gate was evaluated and nothing blocks a publish", while null is "undetermined" — the registry could not evaluate the gate, so null is not evidence that publishing will succeed. Requires platform support for `publish_blockers` on the server response (bdai-platform BCP-3815) (BCP-4012).
* Nightly acceptance coverage now asserts that the registry actually sends both fields on a real server read, on the resource and the data source alike — the unit tests construct the response themselves, so they would stay green if the API dropped either field. `attention_tier` must be present and one of the two tiers a ready, unpublished server can report (`available` with the `mcp-server-publishing` flag off, `pending_publish` with it on); `publish_blockers` is asserted by vocabulary rather than by presence, because null is a legitimate value for it with the flag off and is indistinguishable from an absent key (BCP-4012).

## 0.6.0 (2026-09-21)

FEATURES:

* **New Resource:** `barndoor_mcp_server_publication` — publishes an MCP server, the one-way, admin-initiated step that makes it discoverable to end users. It is a separate resource (not a flag on `barndoor_mcp_server`) because publishing requires an ACTIVE policy, and policies reference the server's id — so the publication is ordered after them by referencing their ids in the optional, ordering-only `policy_ids` attribute (no `depends_on`; editing the list later is an in-place state write, and it imports as null). Creation retries the two precondition rejections (server not operationally available / no ACTIVE policy) for up to two minutes, so a configuration expressing no ordering still converges when the policy lands during the same apply; every other error fails at once. Re-applying is an idempotent no-op; removing the declaration does **not** unpublish (unpublishing does not exist) and never touches the server; import by server id. Requires platform support for the server publish endpoint (bdai-platform BCP-3659 / #6611) (BCP-3734).

ENHANCEMENTS:

* resource/`barndoor_mcp_server`, data-source/`barndoor_mcp_server`: new read-only `published_at` attribute (RFC 3339; null while unpublished), so operators can see and assert whether a server is published. Requires platform support for `published_at` on the server response (bdai-platform BCP-3659 / #6611) (BCP-3734).
* Nightly acceptance coverage now asserts publish-driven discoverability: a server's row in the registry listing carries `published_at` after publishing and not before. It deliberately avoids the `availability_status` filter, whose meaning is switched by an org feature flag and falls back to operational availability — an assertion built on it would be unsound in both directions. The end-user audience-scoped listing cannot be asserted by the provider's machine credential (it has no user record) and is covered by the platform's own e2e suite (BCP-3734).

## 0.5.0 (2026-08-31)

FEATURES:

* **New Resource:** `barndoor_notification_channel` — manages an organization notification channel, the destination Barndoor admin alerts are delivered to. Covers the four organization-wide types: `email` (any deliverable address), `webhook` (an HTTPS endpoint you own, signed with a Standard Webhooks secret), `slack` (a channel in the organization's connected workspace), and `teams` (a Workflows incoming-webhook URL). The `subscriptions` set names the alert types delivered, and is **replaced** rather than merged by the API, so it is the complete desired state. Per-type destination rules are enforced at plan time rather than surfacing as an apply-time 422. The personal channel types (`in_app`, `user_email`) are deliberately not manageable: they are per-user preferences whose owner the API derives from the caller's token, so a Terraform credential could only ever manage its own. Requires platform support for the public notification API (bdai-platform BCP-3758 / #7293).

  A `webhook` channel's signing secret is generated by the platform and revealed **exactly once**, on creation or rotation — it cannot be read back. Following the convention set by `google_service_account_key.private_key` and `azuread_application_password`, `signing_secret` is a computed, sensitive attribute persisted in Terraform state (protect your remote state accordingly) and is never refreshed from the API. Rotation is driven by the `rotate_when_changed` keeper map — and unlike those providers, which must destroy and recreate to rotate, this rotates **in place**: the channel id, subscriptions and delivery configuration are all preserved, because the API exposes a dedicated rotate endpoint. `has_signing_secret` and `has_workflow_url` are refreshed on read and are the reliable signals that a secret exists (BCP-3760).

* resource/`barndoor_log_export`: the `destination` block now supports **Azure Blob Storage** alongside S3, selected with the new `destination.provider` attribute (`s3` — the default — or `azure_blob`). An Azure destination takes the blob service root URL in `endpoint`, the container name in `bucket`, an optional `path_prefix`, and authenticates with the new `account_key` or `sas_token` — sensitive, config-only attributes that the API never returns (and which, like the S3 access keys, are stored in Terraform state); `auth_method` is required for Azure and must be `account_key` or `sas_token` (the `access_keys` default applies to S3 only). The two providers accept disjoint attribute sets, and the mismatches are caught at plan time rather than by an apply-time API error: `region`, `use_ssl`, `use_path_style`, `iam_role_arn`, `access_key_id`, and `secret_access_key` are rejected on an `azure_blob` destination, and `account_key`/`sas_token` on an `s3` one. Existing S3 configurations are unaffected — `provider` defaults to `s3`, and destinations configured before the API had the field read back as `s3`. Requires platform support for Azure destinations, which is gated per organization by a feature flag (bdai-platform BCP-3714 / #7042, #7043).

ENHANCEMENTS:

* data-source/`barndoor_log_export_aws_trust_info`: reading trust info for an export whose destination is Azure Blob Storage now fails with an explanation that the data source only applies to S3 destinations using the `iam_role` auth method, instead of surfacing the API's raw HTTP 400 (BCP-3714).

## 0.4.0 (2026-08-14)

FEATURES:

* **New Resource:** `barndoor_dlp_detection_engine` — manages a Data Protection detection engine (a "Protection Profile" in the platform app): the binding of a detection provider (`provider_type`) to the detection types it scans for, plus an optional provider connection and JSON config. Engines are unique per organization by (`name`, `provider_type`) and every attribute updates in place; deleting an engine that is the only one on an enforcement policy fails with a pointer at the referencing `detection_engine_ids`. Closes the bootstrap gap where `barndoor_dlp_enforcement_policy.detection_engine_ids` could only be filled with UUIDs copied from the app (BCP-3630).
* **New Data Source:** `barndoor_dlp_detection_engine` — looks up an existing detection engine (Protection Profile) by `id` or by `name`, optionally narrowed by `provider_type` (the same name may exist for several provider types — one merged profile in the app); ambiguous names fail loudly with the candidate ids and their provider types (BCP-3630).
* **New Data Source:** `barndoor_llm_provider` — looks up an existing LLM Gateway upstream provider by `id` or `name` (matched case-insensitively, mirroring the API's uniqueness rule), so a provider created in the Barndoor app can be referenced — e.g. to attach model mappings, model-access policies, or pricing rules — without hand-copying its UUID. The provider's credential is never returned by the API and is not part of the data source (BCP-3630).
* **New Data Source:** `barndoor_mcp_server_directory` — looks up an MCP server directory (catalog) entry by `id`, `slug`, or `name`, so `barndoor_mcp_server.mcp_server_directory_id` no longer needs a hand-copied UUID. Neither slugs nor names are unique across the visible catalog (an organization-owned entry may reuse a public connector's slug); ambiguous lookups fail loudly with the candidate ids. Only identification and descriptive metadata is exposed — the entry's OAuth/connection configuration is not. Requires platform support for the public directory endpoints (bdai-platform BCP-3630 / #6794).
* **New Data Source:** `barndoor_agent_directory` — looks up an agent directory entry (the OAuth client definition an agent registration binds to) by `id` or `name`, to feed `barndoor_agent.application_directory_id`. Names carry no uniqueness rule; ambiguous lookups fail loudly with the candidate ids. OAuth client configuration (callbacks, logout URLs) is not exposed, and client secrets are never readable. Requires platform support for the public directory endpoints (bdai-platform BCP-3630 / #6794).

ENHANCEMENTS:

* resource/`barndoor_policy`: rule `actions` entries are now validated at plan time — each must be `*` or a `tools/call:`-prefixed tool name, mirroring the API's format check. Previously a bare tool name passed the plan and failed at apply (BCP-3630).
* provider: transient API failures are now retried with exponential backoff and jitter (up to 4 attempts, honoring `Retry-After`). REST: HTTP 429 retries for every method; 502/503/504 and transport errors retry for idempotent methods (GET/PUT/DELETE), plus dial-phase connection failures for every method. The OAuth token grant retries the same way. gRPC (`barndoor_policy`): `UNAVAILABLE` retries via the channel's retry policy. Previously every request was a single attempt, so a blip mid-apply failed the run (BCP-3630).

NOTES:

* data-source/`barndoor_policy`: the `actions` attribute description now documents the `tools/call:` action format (the resource side was fixed earlier, the data source was missed) (BCP-3630).

* docs/resource/barndoor_connection: documented `terraform import` — the import key is the **server's** UUID or slug (an organization holds at most one tenant-wide connection per server). Previously this was the only resource page without an Import section.
* resource/`barndoor_policy`: the `actions` schema description and the examples now spell out the platform's action format — each entry is `*` or a `tools/call:`-prefixed tool name. The published examples showed bare tool names (e.g. `search`, `create_*`), which the API rejects.
* docs: every configuration under `examples/` is now validated with `terraform validate` in CI (`make validate-examples` runs the same check locally), so invalid example HCL can no longer ship in the published docs. The examples that referenced undeclared variables or resources are now self-contained.

## 0.3.1 (2026-07-15)

BUG FIXES:

* resource/`barndoor_log_export`: `terraform import` now hydrates the `settings` block (`batch_size`, `flush_interval_seconds`, `max_retries`) from the server. Previously import left `settings` null, producing incomplete state and a spurious in-place diff on the next plan.

## 0.3.0 (2026-07-06)

FEATURES:

* **New Resource:** `barndoor_connection` — manages the organization's tenant-wide (service-account-owned) credential connection to an MCP server, for non-OAuth credential providers (`api_key`, `bearer_token`, `basic_auth`, `generic`). Credentials are write-only and any change forces a new connection. OAuth providers are rejected with an explanatory error — their interactive browser consent cannot be performed by a declarative apply. Requires platform support for `as_service` on the connection read endpoint (bdai-platform BCP-3256).
* **New Data Source:** `barndoor_policy` — looks up an existing access policy by `id` or `name` (exact match among non-archived policies) and exposes its full attribute set, including rules.
* **New Data Source:** `barndoor_agent` — looks up an existing AI Agent registration by `id` or display `name`; ambiguous display names fail loudly with the candidate ids.
* **New Data Source:** `barndoor_mcp_server` — looks up an existing MCP server by `id`, `name` (matched case- and whitespace-insensitively, mirroring the API's uniqueness rule), or `slug`. Credential attributes are never part of the data source.
* **New Resource:** `barndoor_dlp_org_config` — manages the organization's singleton Data Protection configuration (`enabled`, `global_dry_run`) over the dlp-service tenant admin REST API. The platform provisions the row per organization, so the resource adopts and configures it; `terraform destroy` resets both settings to the platform defaults (`enabled = true`, `global_dry_run = false`) rather than deleting anything (BCP-3257).
* **New Resource:** `barndoor_dlp_enforcement_policy` — manages a Data Protection enforcement policy: MCP-server or model-provider targeting (with API-side `target_kind` inference), runtime stage, action, priority (API-assigned when unset), dry-run flag, principal scoping, and the detection engines that evaluate the traffic (BCP-3257).
* **New Resource:** `barndoor_dlp_allow_list_entry` — manages one Data Protection allow-list entry (literal or regex pattern, optional detection-type scoping, audit reason). The platform API has no update endpoint and no get-by-id, so every attribute change replaces the entry and reads walk the paginated list (BCP-3257).
* **New Resource:** `barndoor_dlp_custom_detection_type` — manages an organization-defined Data Protection detection type: an ordered list of literal/regex patterns plus default severity and confidence. The platform assigns the type's wire name, exposed as `detection_type` for cross-referencing from allow-list entries (BCP-3257).
* **New Resource:** `barndoor_dlp_field_control_policy` — manages the Data Protection field control policy of one MCP server: per-tool rules (JSON, `jsonencode([...])`) that pass, redact, or block fields of tool output payloads, plus the enabled flag and the server-managed `version` counter. The platform keeps one policy per MCP server and its create endpoint is an upsert, so Create refuses to adopt a pre-existing policy and directs to `terraform import` instead (BCP-3257).
* **New Resource:** `barndoor_llm_provider` — manages an LLM Gateway upstream provider: model-provider family, base URL, auth type, provider settings, the enabled toggle, and the health-check routing gate. The `api_key` credential is write-only (stored in the platform secret store, never echoed); changing it rotates the credential in place. Shared connections and structured non-API-key credentials are out of scope for now (BCP-3259).
* **New Resource:** `barndoor_llm_model_mapping` — manages an LLM Gateway model route from a caller-facing alias to an upstream model on a provider, with failover `priority`, 429 retry policy, bare-name resolution, and per-route timeouts. Priorities are written through the per-mapping update endpoint; the platform's bulk reorder endpoint is not used (BCP-3259).
* **New Resource:** `barndoor_llm_model_access` — manages an LLM Gateway model-access policy: an allowlist/denylist of targets (model aliases, upstream models, providers, or provider+model pairs) applied to an identity scope and traffic lane (BCP-3259).
* **New Resource:** `barndoor_llm_rate_limit` — manages an LLM Gateway rate-limit policy: per-minute request and/or token ceilings on an identity scope. Removing a metric from configuration clears it on the platform (the update API's explicit-null semantics) (BCP-3259).
* **New Resource:** `barndoor_llm_token_budget` — manages an LLM Gateway token budget: a daily/weekly/monthly token ceiling on an identity scope, with alert thresholds and a block/throttle/warn exhaustion action. Scope attributes are immutable and force replacement; cost limits and route-target dimensions are out of scope for now (BCP-3259).
* **New Resource:** `barndoor_idp` — manages the organization's **singleton** enterprise SSO connection over the identity-service public REST API: OIDC IdP federation (issuer, endpoints, write-only client credentials, scopes, SSO email domain) plus the optional IdP-group-to-admin-role mapping (`admin_group`). Create refuses to adopt a pre-existing connection (the API answers 409) and directs to `terraform import`; every attribute updates in place — nothing recreates the connection. SSO enforcement, break-glass accounts, OIDC discovery, and SCIM stay portal-only. Requires platform support for the public IdP surface (bdai-platform BCP-3260 / #5440) (BCP-3261).
* **New Data Source:** `barndoor_idp_settings` — reads the organization's IdP flags (`idp_role_binding_only`, `enforce_sso`, and the break-glass account), which are read-only on the public API surface (BCP-3261).
* **New Resource:** `barndoor_llm_governance_config` — manages the organization's singleton LLM Gateway governance configuration (`require_pricing_for_mappings`). The resource adopts and configures the singleton; `terraform destroy` resets it to the platform defaults rather than deleting anything (BCP-3259).
* **New Resource:** `barndoor_llm_model_pricing` — manages an LLM Gateway model pricing rule over the platform's append-only **versioned** pricing store: the resource owns a rule's current pricing intent identified by `(model_provider, model_pattern)`, updates append new versions (`id` follows the current version row), a future-dated `effective_from` schedules the change (edited in place while still pending), and `terraform destroy` archives the rule (restorable tombstone, full history preserved). Rules default to `sync_mode = "pinned"` so the platform's default-pricing syncs never override Terraform. Imports go by logical identity (`model_provider|model_pattern`). The org-bulk `import-defaults`/`sync-defaults` endpoints are intentionally not bound (BCP-3259).

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
