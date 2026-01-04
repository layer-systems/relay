package main

import (
	"context"

	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
)

// RejectNonAuthenticatedGiftWrapQueries implements NIP-17 metadata protection
// by requiring authentication for kind 1059 (gift wrap) queries and restricting
// results to only show events intended for the authenticated user.
func RejectNonAuthenticatedGiftWrapQueries(ctx context.Context, filter nostr.Filter) (reject bool, msg string) {
	// Check if filter includes kind 1059 (gift wrap events)
	isGiftWrapQuery := false
	for _, kind := range filter.Kinds {
		if kind == 1059 {
			isGiftWrapQuery = true
			break
		}
	}

	if !isGiftWrapQuery {
		return false, "" // not a gift wrap query, allow
	}

	// Require authentication for gift wrap queries
	pubkey := khatru.GetAuthed(ctx)
	if pubkey == "" {
		return true, "auth-required: authentication required to query direct messages"
	}

	// Only allow querying gift wraps addressed to the authenticated user
	// Check if the filter already restricts by recipient pubkey
	hasRecipientFilter := false
	for _, tag := range filter.Tags {
		if len(tag) > 0 && tag[0] == "p" {
			// Check if authenticated user is in the p tags
			for i := 1; i < len(tag); i++ {
				if tag[i] == pubkey {
					hasRecipientFilter = true
					break
				}
			}
		}
	}

	if !hasRecipientFilter {
		// Force the filter to only include events where the authenticated user is the recipient
		if filter.Tags == nil {
			filter.Tags = make(nostr.TagMap)
		}
		filter.Tags["p"] = []string{pubkey}
	}

	return false, ""
}
