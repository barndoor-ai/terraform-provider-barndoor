# A named set of routes that model-access policies can target as one. The
# aliases are the model_alias values of barndoor_llm_model_mapping routes.
resource "barndoor_llm_model_route_group" "frontier" {
  name          = "Frontier models"
  description   = "Routes cleared for production traffic"
  model_aliases = ["gpt-4o", "claude-sonnet"]

  # Recorded in the audit trail; sent with every update while it is set.
  change_note = "Approved in the October model review"
}

# Only the group's current members may be called. Editing the group changes
# what this policy allows, with no edit to the policy.
resource "barndoor_llm_model_access" "frontier_only" {
  name        = "Frontier models only"
  scope_type  = "org"
  policy_type = "allowlist"

  targets = [
    { kind = "route_group", group_id = barndoor_llm_model_route_group.frontier.id },
  ]
}
