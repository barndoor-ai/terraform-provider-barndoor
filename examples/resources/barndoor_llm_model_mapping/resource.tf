# 1:1 enablement: makes gpt-4o servable on the provider (addressable as
# <provider>/gpt-4o; set bare_alias = true to also serve the bare name).
resource "barndoor_llm_model_mapping" "gpt_4o" {
  provider_id    = barndoor_llm_provider.openai.id
  model_alias    = "gpt-4o"
  upstream_model = "gpt-4o"
}

# Custom alias routed to that model. The 1:1 enablement for the
# (provider, upstream_model) pair must exist first — the API rejects
# orphan aliases.
resource "barndoor_llm_model_mapping" "fast" {
  provider_id    = barndoor_llm_provider.openai.id
  model_alias    = "fast"
  upstream_model = "gpt-4o"

  priority                   = 10
  retry_on_429_count         = 3
  retry_on_429_max_wait_secs = 60

  depends_on = [barndoor_llm_model_mapping.gpt_4o]
}
