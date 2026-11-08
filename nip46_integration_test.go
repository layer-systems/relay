package main

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/fiatjaf/eventstore/slicestore"
	"github.com/fiatjaf/khatru"
	"github.com/fiatjaf/khatru/policies"
	"github.com/nbd-wtf/go-nostr"
)

func TestNIP46Transport(t *testing.T) {
	url := os.Getenv("NIP46_RELAY_URL")
	var store *slicestore.SliceStore
	if url == "" {
		relay := khatru.NewRelay()
		store = &slicestore.SliceStore{}
		store.Init()
		relay.StoreEvent = append(relay.StoreEvent, store.SaveEvent)
		relay.QueryEvents = append(relay.QueryEvents, store.QueryEvents)
		relay.RejectEvent = append(relay.RejectEvent, policies.ValidateKind)
		policies.ApplySaneDefaults(relay)
		ProtectGiftWraps(relay)

		server := httptest.NewServer(relay)
		defer server.Close()
		url = "ws" + server.URL[4:]
	}

	clientSecret := nostr.GeneratePrivateKey()
	clientPublic, err := nostr.GetPublicKey(clientSecret)
	if err != nil {
		t.Fatal(err)
	}
	signerSecret := nostr.GeneratePrivateKey()
	signerPublic, err := nostr.GetPublicKey(signerSecret)
	if err != nil {
		t.Fatal(err)
	}

	event := nostr.Event{
		PubKey:    clientPublic,
		CreatedAt: nostr.Now(),
		Kind:      nostr.KindNostrConnect,
		Tags:      nostr.Tags{{"p", signerPublic}},
		Content:   "opaque-nip44-payload",
	}
	if err := event.Sign(clientSecret); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	receiver, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	sub, err := receiver.Subscribe(ctx, nostr.Filters{{
		Kinds:     []int{nostr.KindNostrConnect},
		Tags:      nostr.TagMap{"p": []string{signerPublic}},
		Since:     &event.CreatedAt,
		LimitZero: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsub()
	select {
	case <-sub.EndOfStoredEvents:
	case reason := <-sub.ClosedReason:
		t.Fatalf("subscription closed: %s", reason)
	case <-ctx.Done():
		t.Fatal("timed out waiting for EOSE")
	}

	if err := sender.Publish(ctx, event); err != nil {
		t.Fatalf("publish NIP-46 event: %v", err)
	}

	select {
	case received := <-sub.Events:
		if received.ID != event.ID {
			t.Fatalf("received event %s, want %s", received.ID, event.ID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for NIP-46 event")
	}

	if store != nil {
		queryCtx, queryCancel := context.WithTimeout(context.Background(), time.Second)
		defer queryCancel()
		stored, err := store.QueryEvents(queryCtx, nostr.Filter{IDs: []string{event.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if got := <-stored; got != nil {
			t.Fatalf("ephemeral NIP-46 event was stored: %s", got.ID)
		}
	}
}
