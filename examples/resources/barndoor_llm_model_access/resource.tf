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
    { kind = "provider", provider_id = "11111111-1111-1111-1111-111111111111" }, # a barndoor_llm_provider id
  ]

  traffic_type = "all"
}

# Allow a route group's members for one team. An empty or deleted group
# expands to no targets, so an allowlist whose only target is that group
# denies every model.
resource "barndoor_llm_model_access" "research_routes" {
  name        = "Research routes"
  scope_type  = "group"
  scope_value = "research"
  policy_type = "allowlist"

  targets = [
    { kind = "route_group", group_id = "22222222-2222-2222-2222-222222222222" }, # a barndoor_llm_model_route_group id
  ]
}
