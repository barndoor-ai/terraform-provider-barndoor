# Price every gpt-4-family model on the platform's openai catalog pricing
# scope. Terraform owns the price: sync_mode defaults to "pinned", so the
# platform's default-pricing syncs never touch it.
resource "barndoor_llm_model_pricing" "gpt4" {
  model_pattern  = "gpt-4*"
  model_provider = "openai"

  input_cost_per_million_tokens  = 2.5
  output_cost_per_million_tokens = 10

  change_reason = "Managed by Terraform"
}

# Anthropic-family pricing with prompt-cache overrides and a scheduled price
# change: the platform bills the previous version until effective_from, then
# switches to this one.
resource "barndoor_llm_model_pricing" "claude_sonnet" {
  model_pattern  = "claude-sonnet-*"
  model_provider = "anthropic"

  input_cost_per_million_tokens  = 3
  output_cost_per_million_tokens = 15

  cache_read_cost_per_million_tokens  = 0.3
  cache_write_cost_per_million_tokens = 3.75

  effective_from = "2030-01-01T00:00:00Z"
  change_reason  = "Negotiated 2030 rates"
}
