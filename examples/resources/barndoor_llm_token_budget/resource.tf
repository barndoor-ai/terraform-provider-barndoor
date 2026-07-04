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
