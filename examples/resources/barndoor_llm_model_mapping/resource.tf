# 1:1 enablement: makes gpt-4o servable on the provider (addressable as
# <provider>/gpt-4o; set bare_alias = true to also serve the bare name).
resource "barndoor_llm_model_mapping" "gpt_4o" {
  provider_id    = "11111111-1111-1111-1111-111111111111" # a barndoor_llm_provider id
  model_alias    = "gpt-4o"
  upstream_model = "gpt-4o"
}

# Custom alias routed to that model. The 1:1 enablement for the
# (provider, upstream_model) pair must exist first — the API rejects
# orphan aliases.
resource "barndoor_llm_model_mapping" "fast" {
  provider_id    = "11111111-1111-1111-1111-111111111111" # same provider
  model_alias    = "fast"
  upstream_model = "gpt-4o"

  priority                   = 10
  retry_on_429_count         = 3
  retry_on_429_max_wait_secs = 60

  depends_on = [barndoor_llm_model_mapping.gpt_4o]
}

# A self-hosted reasoning model the gateway can't recognise by name: always
# send the client's reasoning-effort hint (Claude Code's effort level) as
# reasoning_effort. Leave reasoning_effort_support unset to keep whatever is
# set in the Barndoor app; "auto" returns the model to name-based detection.
resource "barndoor_llm_model_mapping" "internal_r1" {
  provider_id    = "22222222-2222-2222-2222-222222222222" # a custom (OpenAI-compatible) provider
  model_alias    = "internal-r1"
  upstream_model = "internal-r1"

  reasoning_effort_support = "supported"
}
