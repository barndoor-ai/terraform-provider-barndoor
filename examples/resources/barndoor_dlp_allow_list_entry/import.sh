# Import by the allow-list entry's UUID (the resource's id). The API has no
# get-by-id endpoint, so the provider finds the entry by walking the paginated
# allow list.
terraform import barndoor_dlp_allow_list_entry.support_mailbox 11111111-1111-1111-1111-111111111111
