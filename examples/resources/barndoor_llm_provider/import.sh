# Import by provider UUID. api_key is write-only and cannot be imported;
# set it in configuration and the next apply rotates the stored credential.
terraform import barndoor_llm_provider.openai 5b1c9c6e-6a51-4f8e-9d0e-1f2a3b4c5d6e
