# Org-wide allowlist: only gpt-* aliases may be called.
resource "barndoor_llm_model_access" "frontier_only" {
  name        = "Frontier models only"
  scope_type  = "org"
  policy_type = "allowlist"

  targets = [
    { kind = "model_alias", alias = "gpt-*" },
  ]
}

# Deny a specific upstream model and everything on one provider for an
# IdP group, across both traffic lanes.
resource "barndoor_llm_model_access" "engineering_denylist" {
  name        = "Engineering denylist"
  scope_type  = "group"
  scope_value = "engineering"
  policy_type = "denylist"

  targets = [
    { kind = "model", model = "gpt-4o-mini" },
    { kind = "provider", provider_id = barndoor_llm_provider.staging.id },
  ]

  traffic_type = "all"
}
