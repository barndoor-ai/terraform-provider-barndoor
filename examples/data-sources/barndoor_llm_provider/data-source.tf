# Look up an existing LLM Gateway provider by name (or by id) to reference it
# without managing it — most commonly to attach model mappings, model-access
# policies, or pricing rules to a provider that was created in the Barndoor
# app. The provider's credential is never part of the data source.
data "barndoor_llm_provider" "openai" {
  name = "OpenAI Production"
}

# Enable gpt-4o on the looked-up provider (the 1:1 enablement mapping).
resource "barndoor_llm_model_mapping" "gpt_4o" {
  provider_id    = data.barndoor_llm_provider.openai.id
  model_alias    = "gpt-4o"
  upstream_model = "gpt-4o"
}

output "openai_health_status" {
  value = data.barndoor_llm_provider.openai.health_status
}
