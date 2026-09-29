# A provider reads its upstream secret from a connection. Several providers
# can share one connection; base_url, unset here, follows the connection's.
resource "barndoor_llm_connection" "openai" {
  name           = "OpenAI production key"
  model_provider = "openai"
  base_url       = "https://api.openai.com"
  api_key        = var.openai_api_key
}

resource "barndoor_llm_provider" "openai" {
  name           = "OpenAI"
  model_provider = "openai"
  connection_id  = barndoor_llm_connection.openai.id
}

# A provider that is configured but not yet serving traffic, with the
# connectivity-probe routing gate bypassed.
resource "barndoor_llm_connection" "anthropic" {
  name           = "Anthropic staging key"
  model_provider = "anthropic"
  base_url       = "https://api.anthropic.com"
  api_key        = var.anthropic_api_key
}

resource "barndoor_llm_provider" "staging" {
  name           = "Anthropic (staging)"
  model_provider = "anthropic"
  connection_id  = barndoor_llm_connection.anthropic.id

  enabled              = false
  enforce_health_check = false
}

# A request-scoped OAuth passthrough: callers bring their own Claude
# subscription, so nothing is stored upstream and no connection is needed.
# The subscription is billed flat, so the provider records no token cost.
resource "barndoor_llm_provider" "claude_subscription" {
  name           = "Claude (subscription)"
  model_provider = "anthropic"
  auth_type      = "claude_oauth"
  base_url       = "https://api.anthropic.com"

  billing_mode   = "not_metered"
  billing_reason = "subscription"
}

variable "openai_api_key" {
  type      = string
  sensitive = true
}

variable "anthropic_api_key" {
  type      = string
  sensitive = true
}
