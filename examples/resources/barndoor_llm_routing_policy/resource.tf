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

  # Requests over 128k tokens go to slot 1 or above, over 512k to slot 2. One
  # fewer breakpoint than slots; this is the default, so it can be omitted for
  # a three-slot policy but must be set for any other slot count.
  context_breakpoints = [128000, 512000]
}

# To make callers go through routing policies instead of naming models
# directly, set require_routing_policy = true on barndoor_llm_governance_config.
