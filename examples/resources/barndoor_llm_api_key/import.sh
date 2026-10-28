# Import by key UUID. The secret can't be imported: the platform never returns
# it again, so `key` stays null until you change `rotation_trigger`.
terraform import barndoor_llm_api_key.billing 1f4a8b2c-3d4e-4f5a-8b9c-0d1e2f3a4b5c
