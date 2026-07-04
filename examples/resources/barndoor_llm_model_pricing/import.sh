# Version ids rotate on every price change, so import goes by the rule's
# logical identity instead: `model_provider|model_pattern`, or just
# `model_pattern` for a provider-unscoped rule.
terraform import barndoor_llm_model_pricing.gpt4 'openai|gpt-4*'
terraform import barndoor_llm_model_pricing.all_models 'fallback-*'
