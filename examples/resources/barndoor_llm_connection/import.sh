# Import by connection UUID. api_key and credentials are write-only and cannot
# be imported; set them in configuration and the next apply rewrites the
# stored secret.
terraform import barndoor_llm_connection.openai 8d2f6a1e-3b4c-4d5e-9f60-7a8b9c0d1e2f
