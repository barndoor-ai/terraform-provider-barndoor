# Org-wide request ceiling. At least one of requests_per_minute /
# tokens_per_minute is required; removing one clears that metric.
resource "barndoor_llm_rate_limit" "org_ceiling" {
  name                = "Org ceiling"
  scope_type          = "org"
  requests_per_minute = 600
}

# Token throughput cap for one IdP group, LLM traffic only.
resource "barndoor_llm_rate_limit" "engineering_tokens" {
  name              = "Engineering token ceiling"
  scope_type        = "group"
  scope_value       = "engineering"
  tokens_per_minute = 250000
  traffic_type      = "llm"
}

# A ceiling on one upstream model, whichever alias the caller used.
# Rate-limit targets update in place, and removing one clears it.
resource "barndoor_llm_rate_limit" "opus_tokens" {
  name                  = "Opus token ceiling"
  scope_type            = "org"
  tokens_per_minute     = 400000
  target_provider_id    = var.anthropic_provider_id
  target_upstream_model = "claude-opus-5-5"
  traffic_type          = "llm"

  change_note = "Protect the shared Opus quota"
}

variable "anthropic_provider_id" {
  type = string
}
