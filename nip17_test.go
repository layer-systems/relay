package main

import (
	"context"
	successor "fiatjaf.com/nostr"
	"github.com/fiatjaf/eventstore/postgresql"
	"os"

	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr/khatru"
	"github.com/fiatjaf/eventstore/slicestore"

	"github.com/nbd-wtf/go-nostr"
)

// These tests talk to a real khatru relay over websockets so that every
// protocol path (REQ, live subscriptions, COUNT, AUTH) is exercised exactly as
// a client would see it.

type keypair struct{ sk, pk string }

func newKeypair(t *testing.T) keypair {
	t.Helper()
	sk := nostr.GeneratePrivateKey()
	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		t.Fatal(err)
	}
	return keypair{sk, pk}
}

func signedEvent(t *testing.T, signer keypair, kind int, tags nostr.Tags, createdAt nostr.Timestamp) *nostr.Event {
	t.Helper()
	evt := &nostr.Event{
		PubKey:    signer.pk,
		CreatedAt: createdAt,
		Kind:      kind,
		Tags:      tags,
		Content:   "opaque",
	}
	if err := evt.Sign(signer.sk); err != nil {
		t.Fatal(err)
	}
	return evt
}

// giftWrap builds a kind 1059 envelope signed by a random key, as NIP-59 does.
func giftWrap(t *testing.T, createdAt nostr.Timestamp, recipients ...string) *nostr.Event {
	t.Helper()
	tags := nostr.Tags{}
	for _, r := range recipients {
		tags = append(tags, nostr.Tag{"p", r})
	}
	return signedEvent(t, newKeypair(t), KindGiftWrap, tags, createdAt)
}

type testRelay struct {
	url   string
	store legacyStore
}

func newTestRelay(t *testing.T) *testRelay {
	t.Helper()
	relay := khatru.NewRelay()
	var store legacyStore
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		db := &postgresql.PostgresBackend{DatabaseURL: url}
		if err := db.Init(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(db.Close)
		if _, err := db.Exec("TRUNCATE event"); err != nil {
			t.Fatal(err)
		}
		store = db
	} else {
		db := &slicestore.SliceStore{}
		if err := db.Init(); err != nil {
			t.Fatal(err)
		}
		store = db
	}
	wireStore(relay, store)
	ProtectGiftWraps(relay)

	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	return &testRelay{url: "ws" + strings.TrimPrefix(server.URL, "http"), store: store}
}

func (tr *testRelay) seed(t *testing.T, events ...*nostr.Event) {
	t.Helper()
	for _, evt := range events {
		if err := tr.store.SaveEvent(context.Background(), evt); err != nil {
			t.Fatal(err)
		}
	}
}

func (tr *testRelay) connect(t *testing.T) *nostr.Relay {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := nostr.RelayConnect(ctx, tr.url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// connectAs opens a connection and completes NIP-42 AUTH as user. khatru only
// sends a challenge after an auth-required rejection, so we trigger one first.
func (tr *testRelay) connectAs(t *testing.T, user keypair) *nostr.Relay {
	t.Helper()
	conn := tr.connect(t)
	_, closed := collect(t, conn, nostr.Filter{Kinds: []int{KindGiftWrap}})
	if !strings.HasPrefix(closed, "auth-required:") {
		t.Fatalf("expected auth-required before AUTH, got %q", closed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Auth(ctx, func(evt *nostr.Event) error { return evt.Sign(user.sk) }); err != nil {
		t.Fatalf("auth: %v", err)
	}
	return conn
}

// collect runs a REQ with the given filters and returns the IDs received before
// EOSE, or the CLOSED reason if the relay refused the subscription.
func collect(t *testing.T, conn *nostr.Relay, filters ...nostr.Filter) (ids []string, closed string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := conn.Subscribe(ctx, filters)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsub()
	for {
		select {
		case evt := <-sub.Events:
			ids = append(ids, evt.ID)
		case <-sub.EndOfStoredEvents:
			// drain anything already buffered
			for {
				select {
				case evt := <-sub.Events:
					ids = append(ids, evt.ID)
				default:
					return ids, ""
				}
			}
		case reason := <-sub.ClosedReason:
			return ids, reason
		case <-ctx.Done():
			t.Fatal("timed out waiting for EOSE")
		}
	}
}

func count(t *testing.T, conn *nostr.Relay, filter nostr.Filter) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, _, err := conn.Count(ctx, nostr.Filters{filter})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func assertIDs(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s: got %v, want %v", name, got, want)
	}
}

type fixture struct {
	alice, bob, carol keypair
	aliceWrap         *nostr.Event // addressed to alice
	bobWrap           *nostr.Event // addressed to bob
	sharedWrap        *nostr.Event // p-tagged alice and carol
	note              *nostr.Event // public kind 1 by carol mentioning alice
}

func newFixture(t *testing.T, tr *testRelay) fixture {
	t.Helper()
	now := nostr.Now()
	f := fixture{alice: newKeypair(t), bob: newKeypair(t), carol: newKeypair(t)}
	f.aliceWrap = giftWrap(t, now-30, f.alice.pk)
	f.bobWrap = giftWrap(t, now-20, f.bob.pk)
	f.sharedWrap = giftWrap(t, now-10, f.alice.pk, f.carol.pk)
	f.note = signedEvent(t, f.carol, 1, nostr.Tags{{"p", f.alice.pk}}, now-5)
	tr.seed(t, f.aliceWrap, f.bobWrap, f.sharedWrap, f.note)
	return f
}

func TestGiftWrapStoredUnauthenticated(t *testing.T) {
	tr := newTestRelay(t)
	f := newFixture(t, tr)
	conn := tr.connect(t)

	allIDs := []string{f.aliceWrap.ID, f.bobWrap.ID, f.sharedWrap.ID, f.note.ID}

	ids, closed := collect(t, conn, nostr.Filter{Kinds: []int{KindGiftWrap}, Tags: nostr.TagMap{"p": {f.alice.pk}}})
	if !strings.HasPrefix(closed, "auth-required:") || len(ids) != 0 {
		t.Fatalf("explicit gift wrap query: closed=%q ids=%v", closed, ids)
	}

	cases := []struct {
		name    string
		filters []nostr.Filter
		want    []string
	}{
		{"omitted kinds", []nostr.Filter{{}}, []string{f.note.ID}},
		{"omitted kinds with #p", []nostr.Filter{{Tags: nostr.TagMap{"p": {f.alice.pk}}}}, []string{f.note.ID}},
		{"ID-only", []nostr.Filter{{IDs: allIDs}}, []string{f.note.ID}},
		{"author of wrap", []nostr.Filter{{Authors: []string{f.aliceWrap.PubKey}}}, nil},
		{"multiple filters", []nostr.Filter{{IDs: []string{f.aliceWrap.ID}}, {Kinds: []int{1}}}, []string{f.note.ID}},
		{"public kind", []nostr.Filter{{Kinds: []int{1}}}, []string{f.note.ID}},
	}
	for _, tc := range cases {
		ids, closed := collect(t, conn, tc.filters...)
		if closed != "" {
			t.Errorf("%s: unexpected CLOSED %q", tc.name, closed)
		}
		assertIDs(t, tc.name, ids, tc.want...)
	}
}

func TestGiftWrapStoredWrongRecipient(t *testing.T) {
	tr := newTestRelay(t)
	f := newFixture(t, tr)
	conn := tr.connectAs(t, f.bob)

	// Asking only for someone else's gift wraps is refused outright.
	ids, closed := collect(t, conn, nostr.Filter{Kinds: []int{KindGiftWrap}, Tags: nostr.TagMap{"p": {f.alice.pk}}})
	if !strings.HasPrefix(closed, "restricted:") || len(ids) != 0 {
		t.Fatalf("other recipient query: closed=%q ids=%v", closed, ids)
	}

	cases := []struct {
		name    string
		filters []nostr.Filter
		want    []string
	}{
		{"kind 1059 without #p", []nostr.Filter{{Kinds: []int{KindGiftWrap}}}, []string{f.bobWrap.ID}},
		{"multiple recipient values", []nostr.Filter{{Kinds: []int{KindGiftWrap}, Tags: nostr.TagMap{"p": {f.bob.pk, f.alice.pk, f.carol.pk}}}}, []string{f.bobWrap.ID}},
		{"mixed kinds", []nostr.Filter{{Kinds: []int{1, KindGiftWrap}}}, []string{f.bobWrap.ID, f.note.ID}},
		{"mixed kinds, other recipient", []nostr.Filter{{Kinds: []int{1, KindGiftWrap}, Tags: nostr.TagMap{"p": {f.alice.pk}}}}, []string{f.note.ID}},
		{"omitted kinds", []nostr.Filter{{}}, []string{f.bobWrap.ID, f.note.ID}},
		{"ID-only for other's wraps", []nostr.Filter{{IDs: []string{f.aliceWrap.ID, f.sharedWrap.ID}}}, nil},
		{"multiple filters", []nostr.Filter{{IDs: []string{f.aliceWrap.ID}}, {Authors: []string{f.sharedWrap.PubKey}}, {Kinds: []int{KindGiftWrap}}}, []string{f.bobWrap.ID}},
	}
	for _, tc := range cases {
		ids, closed := collect(t, conn, tc.filters...)
		if closed != "" {
			t.Errorf("%s: unexpected CLOSED %q", tc.name, closed)
		}
		assertIDs(t, tc.name, ids, tc.want...)
	}
}

func TestGiftWrapStoredRecipientAccess(t *testing.T) {
	tr := newTestRelay(t)
	f := newFixture(t, tr)
	alice := tr.connectAs(t, f.alice)
	carol := tr.connectAs(t, f.carol)

	ids, closed := collect(t, alice, nostr.Filter{Kinds: []int{KindGiftWrap}, Tags: nostr.TagMap{"p": {f.alice.pk}}})
	if closed != "" {
		t.Fatalf("recipient query closed: %q", closed)
	}
	assertIDs(t, "alice #p", ids, f.aliceWrap.ID, f.sharedWrap.ID)

	ids, _ = collect(t, alice, nostr.Filter{IDs: []string{f.aliceWrap.ID, f.bobWrap.ID}})
	assertIDs(t, "alice ID-only", ids, f.aliceWrap.ID)

	// carol is the second p tag of the shared wrap
	ids, _ = collect(t, carol, nostr.Filter{Kinds: []int{KindGiftWrap}})
	assertIDs(t, "carol second recipient", ids, f.sharedWrap.ID)
}

func TestGiftWrapCount(t *testing.T) {
	tr := newTestRelay(t)
	f := newFixture(t, tr)
	anon := tr.connect(t)
	bob := tr.connectAs(t, f.bob)
	alice := tr.connectAs(t, f.alice)

	cases := []struct {
		name   string
		conn   *nostr.Relay
		filter nostr.Filter
		want   int64
	}{
		{"anon omitted kinds", anon, nostr.Filter{}, 1},
		{"anon ID-only", anon, nostr.Filter{IDs: []string{f.aliceWrap.ID, f.note.ID}}, 1},
		{"anon explicit kind", anon, nostr.Filter{Kinds: []int{KindGiftWrap}}, 0},
		{"anon public kind", anon, nostr.Filter{Kinds: []int{1}}, 1},
		{"bob omitted kinds", bob, nostr.Filter{}, 2},
		{"bob kind 1059", bob, nostr.Filter{Kinds: []int{KindGiftWrap}}, 1},
		{"bob mixed recipients", bob, nostr.Filter{Kinds: []int{1, KindGiftWrap}, Tags: nostr.TagMap{"p": {f.bob.pk, f.alice.pk}}}, 2},
		{"bob other recipient", bob, nostr.Filter{Tags: nostr.TagMap{"p": {f.alice.pk}}}, 1},
		{"alice kind 1059", alice, nostr.Filter{Kinds: []int{KindGiftWrap}}, 2},
		{"alice #p", alice, nostr.Filter{Tags: nostr.TagMap{"p": {f.alice.pk}}}, 3},
	}
	for _, tc := range cases {
		if got := count(t, tc.conn, tc.filter); got != tc.want {
			t.Errorf("%s: count %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestGiftWrapLiveSubscriptions(t *testing.T) {
	tr := newTestRelay(t)
	alice, bob := newKeypair(t), newKeypair(t)

	anon := tr.connect(t)
	bobConn := tr.connectAs(t, bob)
	aliceConn := tr.connectAs(t, alice)
	sender := tr.connect(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	since := nostr.Now() - 1

	subscribe := func(conn *nostr.Relay, filters ...nostr.Filter) *nostr.Subscription {
		t.Helper()
		sub, err := conn.Subscribe(ctx, filters)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sub.Unsub)
		select {
		case <-sub.EndOfStoredEvents:
		case reason := <-sub.ClosedReason:
			t.Fatalf("subscription closed: %s", reason)
		case <-ctx.Done():
			t.Fatal("timed out waiting for EOSE")
		}
		return sub
	}

	// Each listener matches both the gift wrap and the public note. limit:0
	// subscriptions skip RejectFilter entirely in khatru, so they must be
	// covered by the broadcast guard.
	subs := map[string]*nostr.Subscription{
		"anon omitted kinds":        subscribe(anon, nostr.Filter{Since: &since}),
		"anon limit 0 kinds 1,1059": subscribe(anon, nostr.Filter{Kinds: []int{1, KindGiftWrap}, Since: &since, LimitZero: true}),
		"anon kind 1059 limit 0":    subscribe(anon, nostr.Filter{Kinds: []int{KindGiftWrap}, Since: &since, LimitZero: true}, nostr.Filter{Kinds: []int{1}, Since: &since}),
		"bob mixed recipients":      subscribe(bobConn, nostr.Filter{Kinds: []int{1, KindGiftWrap}, Tags: nostr.TagMap{"p": {bob.pk, alice.pk}}, Since: &since}),
		"bob multiple filters":      subscribe(bobConn, nostr.Filter{Kinds: []int{KindGiftWrap}, Since: &since}, nostr.Filter{Kinds: []int{1}, Since: &since}),
		"alice recipient":           subscribe(aliceConn, nostr.Filter{Kinds: []int{1, KindGiftWrap}, Tags: nostr.TagMap{"p": {alice.pk}}, Since: &since}),
		"alice omitted kinds":       subscribe(aliceConn, nostr.Filter{Since: &since}),
	}

	wrap := giftWrap(t, nostr.Now(), alice.pk)
	note := signedEvent(t, bob, 1, nostr.Tags{{"p", alice.pk}}, nostr.Now())
	for _, evt := range []*nostr.Event{wrap, note} {
		if err := sender.Publish(ctx, *evt); err != nil {
			t.Fatalf("publish kind %d: %v", evt.Kind, err)
		}
	}

	// The wrap is broadcast before the note and each connection's writes are
	// ordered, so once the note arrives we know whether the wrap leaked.
	for name, sub := range subs {
		var got []string
		for !slices.Contains(got, note.ID) {
			select {
			case evt := <-sub.Events:
				got = append(got, evt.ID)
			case <-ctx.Done():
				t.Fatalf("%s: timed out, got %v", name, got)
			}
		}
		if strings.HasPrefix(name, "alice") {
			assertIDs(t, name, got, wrap.ID, note.ID)
		} else {
			assertIDs(t, name, got, note.ID)
		}
	}
}

func TestCanReadGiftWrap(t *testing.T) {
	alice, bob := newKeypair(t), newKeypair(t)
	wrap := giftWrap(t, nostr.Now(), alice.pk)
	if legacyCanReadGiftWrap("", wrap) || legacyCanReadGiftWrap(bob.pk, wrap) || !legacyCanReadGiftWrap(alice.pk, wrap) {
		t.Fatal("gift wrap readable by wrong user")
	}
	// a "p" tag key without a value must not match the empty pubkey
	malformed := signedEvent(t, bob, KindGiftWrap, nostr.Tags{{"p"}, {"P", alice.pk}}, nostr.Now())
	if legacyCanReadGiftWrap("", malformed) || legacyCanReadGiftWrap(alice.pk, malformed) {
		t.Fatal("malformed gift wrap readable")
	}
	if !legacyCanReadGiftWrap("", signedEvent(t, bob, 1, nil, nostr.Now())) {
		t.Fatal("public event not readable")
	}
}

func legacyCanReadGiftWrap(authed string, e *nostr.Event) bool {
	converted, err := convert[*nostr.Event, successor.Event](e)
	if err != nil {
		return false
	}
	ctx := context.Background()
	if authed != "" {
		pk, err := successor.PubKeyFromHex(authed)
		if err != nil {
			return false
		}
		ctx = khatru.ForceSetAuthed(ctx, pk)
	}
	return canRead(ctx, converted)
}
