# The organization's singleton LLM governance configuration. `terraform
# destroy` resets it to the platform defaults instead of deleting anything.
resource "barndoor_llm_governance_config" "org" {
  # Refuse model routes that have no matching pricing rule, so every request
  # is billable against token budgets.
  require_pricing_for_mappings = true
}
