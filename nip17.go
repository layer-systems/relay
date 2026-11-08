package main

import (
	"context"
	"slices"

	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip45/hyperloglog"
)

// KindGiftWrap is the NIP-59 gift wrap kind used to deliver NIP-17 messages.
const KindGiftWrap = nostr.KindGiftWrap // 1059

// NIP-17 access control
//
// A kind 1059 gift wrap may only be served to an authenticated (NIP-42) client
// whose pubkey appears in one of the gift wrap's "p" tags. Every other event is
// unaffected.
//
// The policy is enforced at the event level on every path that can expose a
// stored or live gift wrap, so it does not depend on how a filter is written
// (omitted kinds, ID-only filters, mixed kinds, multiple "#p" values, ...):
//
//   - stored REQ results and NIP-77 negentropy: every QueryEvents function is
//     wrapped and gift wraps the client may not see are dropped.
//   - live subscriptions: a PreventBroadcast hook drops unauthorized gift wraps
//     (khatru skips RejectFilter for limit:0 subscriptions, so this is the only
//     reliable live guard).
//   - COUNT (NIP-45): every CountEvents function is wrapped so that only gift
//     wraps addressed to the authenticated user contribute to the total.
//
// On top of that, RejectFilter / RejectCountFilter give clear, early feedback
// for filters that explicitly ask for gift wraps: unauthenticated clients get
// "auth-required:" (which triggers a NIP-42 challenge) and filters that only
// ask for kind 1059 addressed to other people are refused. These checks are a
// convenience; the event-level guards above are what actually protect the data.
//
// ProtectGiftWraps must be called after every QueryEvents / CountEvents source
// has been registered on the relay (including any future search or sync
// backends), otherwise sources appended later are not wrapped.

// ProtectGiftWraps installs the NIP-17 gift wrap access policy on relay.
func ProtectGiftWraps(relay *khatru.Relay) {
	for i, query := range relay.QueryEvents {
		relay.QueryEvents[i] = guardGiftWrapQuery(query)
	}
	for i, count := range relay.CountEvents {
		relay.CountEvents[i] = guardGiftWrapCount(count)
	}
	// NIP-45 HyperLogLog counting is only used for a few specific kinds (never
	// 1059), but guard it anyway so a future change cannot leak gift wrap counts.
	for i, countHLL := range relay.CountEventsHLL {
		inner := countHLL
		relay.CountEventsHLL[i] = func(ctx context.Context, filter nostr.Filter, offset int) (int64, *hyperloglog.HyperLogLog, error) {
			if filterMayMatchGiftWraps(filter) {
				// fall back to an exact count without HLL registers
				var total int64
				for _, count := range relay.CountEvents {
					n, err := count(ctx, filter)
					if err != nil {
						return 0, nil, err
					}
					total += n
				}
				return total, nil, nil
			}
			return inner(ctx, filter, offset)
		}
	}

	relay.RejectFilter = append(relay.RejectFilter, RejectUnauthorizedGiftWrapFilters)
	relay.RejectCountFilter = append(relay.RejectCountFilter, RejectUnauthorizedGiftWrapFilters)
	relay.PreventBroadcast = append(relay.PreventBroadcast, PreventUnauthorizedGiftWrapBroadcast)
}

// CanReadGiftWrap reports whether a client authenticated as authed may receive
// event. Non-gift-wrap events are always readable by this policy.
func CanReadGiftWrap(authed string, event *nostr.Event) bool {
	if event.Kind != KindGiftWrap {
		return true
	}
	if authed == "" {
		return false
	}
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "p" && tag[1] == authed {
			return true
		}
	}
	return false
}

// RejectUnauthorizedGiftWrapFilters gives early feedback for filters that
// explicitly request kind 1059. It never mutates the filter; filters that may
// match gift wraps without naming kind 1059 are handled by the event-level
// guards installed by ProtectGiftWraps.
func RejectUnauthorizedGiftWrapFilters(ctx context.Context, filter nostr.Filter) (reject bool, msg string) {
	if !slices.Contains(filter.Kinds, KindGiftWrap) {
		return false, ""
	}

	authed := khatru.GetAuthed(ctx)
	if authed == "" {
		return true, "auth-required: authentication required to query direct messages"
	}

	// A filter that asks only for gift wraps addressed exclusively to other
	// people can never return anything; refuse it explicitly.
	if len(filter.Kinds) == 1 {
		if recipients, ok := filter.Tags["p"]; ok && !slices.Contains(recipients, authed) {
			return true, "restricted: you may only query direct messages addressed to you"
		}
	}

	return false, ""
}

// PreventUnauthorizedGiftWrapBroadcast stops live delivery of gift wraps to
// subscribers that are not authenticated as one of the recipients.
func PreventUnauthorizedGiftWrapBroadcast(ws *khatru.WebSocket, event *nostr.Event) bool {
	return !CanReadGiftWrap(ws.AuthedPublicKey, event)
}

type queryFunc = func(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error)
type countFunc = func(ctx context.Context, filter nostr.Filter) (int64, error)

func guardGiftWrapQuery(inner queryFunc) queryFunc {
	return func(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
		ch, err := inner(ctx, filter)
		if err != nil || ch == nil {
			return ch, err
		}
		// Internal lookups (deletion, expiration) must see every event.
		if khatru.IsInternalCall(ctx) || !filterMayMatchGiftWraps(filter) {
			return ch, nil
		}

		authed := khatru.GetAuthed(ctx)
		out := make(chan *nostr.Event)
		go func() {
			defer close(out)
			for event := range ch {
				if !CanReadGiftWrap(authed, event) {
					continue
				}
				select {
				case out <- event:
				case <-ctx.Done():
					// keep draining so the inner producer can finish
					for range ch {
					}
					return
				}
			}
		}()
		return out, nil
	}
}

// guardGiftWrapCount returns
//
//	count(filter) - count(filter ∧ kind 1059) + count(filter ∧ kind 1059 ∧ #p = authed)
//
// i.e. the original total with every gift wrap removed except the ones
// addressed to the authenticated user. Because filter fields are ANDed and
// values within a field are ORed, "filter ∧ #p = authed" is exact: if the
// filter already has a "#p" list it is intersected with {authed}.
func guardGiftWrapCount(inner countFunc) countFunc {
	return func(ctx context.Context, filter nostr.Filter) (int64, error) {
		if khatru.IsInternalCall(ctx) || !filterMayMatchGiftWraps(filter) {
			return inner(ctx, filter)
		}

		total, err := inner(ctx, filter)
		if err != nil {
			return 0, err
		}

		giftWraps := filter.Clone()
		giftWraps.Kinds = []int{KindGiftWrap}
		hidden, err := inner(ctx, giftWraps)
		if err != nil {
			return 0, err
		}
		total -= hidden

		if authed := khatru.GetAuthed(ctx); authed != "" {
			if recipients, ok := giftWraps.Tags["p"]; !ok || slices.Contains(recipients, authed) {
				mine := giftWraps.Clone()
				if mine.Tags == nil {
					mine.Tags = nostr.TagMap{}
				}
				mine.Tags["p"] = []string{authed}
				visible, err := inner(ctx, mine)
				if err != nil {
					return 0, err
				}
				total += visible
			}
		}

		if total < 0 {
			total = 0
		}
		return total, nil
	}
}

// filterMayMatchGiftWraps reports whether filter could match a kind 1059
// event. Filters without kinds match every kind.
func filterMayMatchGiftWraps(filter nostr.Filter) bool {
	return len(filter.Kinds) == 0 || slices.Contains(filter.Kinds, KindGiftWrap)
}
