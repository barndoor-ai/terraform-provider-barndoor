# Slot indices refer to the policy's slots list: 0 = cheap, 1 = mid,
# 2 = strong.
resource "barndoor_llm_routing_rule" "legal" {
  policy_id   = "3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f" # a barndoor_llm_routing_policy id
  name        = "Legal review"
  description = "Contract review or legal analysis must use at least the mid slot"
  floor_slot  = 1
}

# Keep bulk translation off the most expensive slot.
resource "barndoor_llm_routing_rule" "bulk_translation" {
  policy_id   = "3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f"
  name        = "Bulk translation"
  description = "Translating large batches of documents or strings between languages"
  deny_slots  = [2]
}
