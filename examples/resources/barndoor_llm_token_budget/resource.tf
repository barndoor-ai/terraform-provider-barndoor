# Org-wide monthly token budget with the default [80, 90] alert
# thresholds and hard blocking on exhaustion.
resource "barndoor_llm_token_budget" "org_monthly" {
  name        = "Org monthly cap"
  scope_type  = "org"
  period      = "monthly"
  token_limit = 500000000
}

# A softer per-group weekly budget: alert early, warn instead of blocking.
resource "barndoor_llm_token_budget" "contractors_weekly" {
  name        = "Contractors weekly cap"
  scope_type  = "group"
  scope_value = "contractors"
  period      = "weekly"
  token_limit = 5000000

  alert_thresholds  = [50, 75, 95]
  action_on_exhaust = "warn"
  traffic_type      = "llm"
}

# A spend cap: cost_limit with no token_limit makes a cost-only budget,
# priced from the organization's model pricing rules.
resource "barndoor_llm_token_budget" "org_spend" {
  name       = "Org monthly spend"
  scope_type = "org"
  period     = "monthly"
  cost_limit = 25000
  currency   = "USD"

  change_note = "FY27 budget"
}

# A per-model allowance: every user may spend at most $50 a day on the
# "frontier" alias, whatever routes serve it. Targets cannot be updated, so
# changing one replaces the budget.
resource "barndoor_llm_token_budget" "frontier_daily" {
  name               = "Frontier daily allowance"
  scope_type         = "user"
  period             = "daily"
  cost_limit         = 50
  target_model_alias = "frontier"
  traffic_type       = "llm"
}
