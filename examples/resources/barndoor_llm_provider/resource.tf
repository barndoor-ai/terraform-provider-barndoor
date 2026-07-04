# An OpenAI-family provider with a direct API key. The credential is
# write-only: the platform stores it in its secret store and never returns
# it, so keep it out of committed configuration (a variable, or TF_VAR_*).
resource "barndoor_llm_provider" "openai" {
  name           = "OpenAI"
  model_provider = "openai"
  base_url       = "https://api.openai.com/v1"
  api_key        = var.openai_api_key
}

# A provider that is configured but not yet serving traffic, with the
# connectivity-probe routing gate bypassed.
resource "barndoor_llm_provider" "staging" {
  name           = "Anthropic (staging)"
  model_provider = "anthropic"
  base_url       = "https://api.anthropic.com"
  api_key        = var.anthropic_api_key

  enabled              = false
  enforce_health_check = false
}
