# Import by provider UUID. The upstream secret lives on the provider's
# barndoor_llm_connection, so nothing write-only is lost on import.
terraform import barndoor_llm_provider.openai 5b1c9c6e-6a51-4f8e-9d0e-1f2a3b4c5d6e
