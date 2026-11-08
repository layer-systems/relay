package main

import (
	"context"
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"iter"
)

const KindGiftWrap = 1059

func canRead(ctx context.Context, e nostr.Event) bool {
	if e.Kind != KindGiftWrap || khatru.IsInternalCall(ctx) {
		return true
	}
	for _, tag := range e.Tags {
		if len(tag) >= 2 && tag[0] == "p" {
			pk, err := nostr.PubKeyFromHex(tag[1])
			if err == nil && khatru.IsAuthed(ctx, pk) {
				return true
			}
		}
	}
	return false
}
func ProtectGiftWraps(r *khatru.Relay) {
	r.OnRequest = func(ctx context.Context, f nostr.Filter) (bool, string) {
		if f.LimitZero {
			return false, ""
		}
		return rejectGiftWrapRequest(ctx, f)
	}
	r.OnCount = rejectGiftWrapRequest
	query := r.QueryStored
	r.QueryStored = func(ctx context.Context, f nostr.Filter) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) {
			for e := range query(ctx, f) {
				if canRead(ctx, e) && !yield(e) {
					return
				}
			}
		}
	}
	r.PreventBroadcast = func(ws *khatru.WebSocket, _ nostr.Filter, e nostr.Event) bool {
		if e.Kind != KindGiftWrap {
			return false
		}
		for _, pk := range ws.AuthedPublicKeys {
			for _, tag := range e.Tags {
				if len(tag) >= 2 && tag[0] == "p" && tag[1] == pk.Hex() {
					return false
				}
			}
		}
		return true
	}
	count := r.Count
	r.Count = func(ctx context.Context, f nostr.Filter) (uint32, error) {
		if khatru.IsInternalCall(ctx) {
			return count(ctx, f)
		}
		may := len(f.Kinds) == 0
		for _, k := range f.Kinds {
			if k == KindGiftWrap {
				may = true
			}
		}
		if !may {
			return count(ctx, f)
		}
		total, err := count(ctx, f)
		if err != nil {
			return 0, err
		}
		wraps := f.Clone()
		wraps.Kinds = []nostr.Kind{KindGiftWrap}
		hidden, err := count(ctx, wraps)
		if err != nil {
			return 0, err
		}
		total -= hidden
		// Each gift wrap is counted once even when several authenticated keys occur.
		var recipients []string
		for _, pk := range khatru.GetAllAuthed(ctx) {
			recipients = append(recipients, pk.Hex())
		}
		if len(recipients) == 0 {
			return total, nil
		}
		if existing, ok := wraps.Tags["p"]; ok {
			var intersection []string
			for _, p := range existing {
				for _, own := range recipients {
					if p == own {
						intersection = append(intersection, p)
						break
					}
				}
			}
			recipients = intersection
		}
		if len(recipients) == 0 {
			return total, nil
		}
		if wraps.Tags == nil {
			wraps.Tags = nostr.TagMap{}
		}
		wraps.Tags["p"] = recipients
		visible, err := count(ctx, wraps)
		return total + visible, err
	}
}

func rejectGiftWrapRequest(ctx context.Context, f nostr.Filter) (bool, string) {
	explicit := false
	for _, k := range f.Kinds {
		if k == KindGiftWrap {
			explicit = true
		}
	}
	if !explicit {
		return false, ""
	}
	if len(khatru.GetAllAuthed(ctx)) == 0 {
		return true, "auth-required: authenticate to read gift wraps"
	}
	if len(f.Kinds) == 1 {
		if recipients, ok := f.Tags["p"]; ok {
			for _, p := range recipients {
				pk, err := nostr.PubKeyFromHex(p)
				if err == nil && khatru.IsAuthed(ctx, pk) {
					return false, ""
				}
			}
			return true, "restricted: recipient only"
		}
	}
	return false, ""
}
