package main

// The JSON boundary deliberately keeps the legacy PostgreSQL schema and backend
// isolated from the successor relay types. No stored data is rewritten.
import (
	"context"
	"encoding/json"
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	old "github.com/nbd-wtf/go-nostr"
	"iter"
)

type legacyStore interface {
	SaveEvent(context.Context, *old.Event) error
	ReplaceEvent(context.Context, *old.Event) error
	DeleteEvent(context.Context, *old.Event) error
	QueryEvents(context.Context, old.Filter) (chan *old.Event, error)
	CountEvents(context.Context, old.Filter) (int64, error)
}

func convert[A, B any](a A) (b B, err error) {
	data, err := json.Marshal(a)
	if err != nil {
		return b, err
	}
	err = json.Unmarshal(data, &b)
	return
}
func wireStore(r *khatru.Relay, s legacyStore) {
	r.StoreEvent = func(ctx context.Context, e nostr.Event) error {
		v, err := convert[nostr.Event, old.Event](e)
		if err != nil {
			return err
		}
		return s.SaveEvent(ctx, &v)
	}
	r.ReplaceEvent = func(ctx context.Context, e nostr.Event) error {
		v, err := convert[nostr.Event, old.Event](e)
		if err != nil {
			return err
		}
		return s.ReplaceEvent(ctx, &v)
	}
	r.DeleteEvent = func(ctx context.Context, id nostr.ID) error { return s.DeleteEvent(ctx, &old.Event{ID: id.Hex()}) }
	r.QueryStored = func(ctx context.Context, f nostr.Filter) iter.Seq[nostr.Event] {
		return func(yield func(nostr.Event) bool) {
			v, err := convert[nostr.Filter, old.Filter](f)
			if err != nil {
				return
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			ch, err := s.QueryEvents(ctx, v)
			if err != nil {
				r.Log.Printf("query: %v", err)
				return
			}
			for e := range ch {
				n, err := convert[*old.Event, nostr.Event](e)
				if err != nil {
					r.Log.Printf("decode: %v", err)
					continue
				}
				if !yield(n) {
					return
				}
			}
		}
	}
	r.Count = func(ctx context.Context, f nostr.Filter) (uint32, error) {
		v, err := convert[nostr.Filter, old.Filter](f)
		if err != nil {
			return 0, err
		}
		n, err := s.CountEvents(ctx, v)
		return uint32(n), err
	}
}
