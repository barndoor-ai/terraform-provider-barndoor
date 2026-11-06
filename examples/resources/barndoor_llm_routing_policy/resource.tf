# Callers send `model = "auto"` and the gateway picks one of three slots per
# request. Each slot names a bare-callable alias (here, custom
# barndoor_llm_model_mapping aliases), ordered cheapest first.
resource "barndoor_llm_routing_policy" "auto" {
  model_alias            = "auto"
  description            = "Cheap by default, stronger models when the request needs them"
  determiner_model_alias = "cheap" # the model that picks a slot for each request
  posture                = "balanced"

  slots = [
    { model_alias = "cheap", label = "Cheap", description = "Chit-chat, short rewrites, lookups" },
    { model_alias = "mid", label = "Mid", description = "Everyday coding and analysis" },
    { model_alias = "strong", label = "Strong", description = "Hard reasoning, long documents" },
  ]
}

# To make callers go through routing policies instead of naming models
# directly, set require_routing_policy = true on barndoor_llm_governance_config.
