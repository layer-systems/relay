package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/fiatjaf/khatru"
	"github.com/nbd-wtf/go-nostr"
)

// ---------------------------------------------------------------------------
// Unit tests (no database required)
// ---------------------------------------------------------------------------

func TestNormalizeSearchQuery(t *testing.T) {
	cases := map[string]string{
		"best nostr apps":    "best nostr apps",
		"  spaced\t\nout  ":  "spaced out",
		"nostr include:spam": "nostr",
		"nostr DOMAIN:example.com nsfw:false language:en sentiment:positive": "nostr",
		"include:spam":        "",
		"https://example.com": "https://example.com", // not an extension
		"key:value":           "key:value",           // unknown keys are searched
		"nul\x00byte":         "nul byte",
		"bad\xffutf8":         "bad utf8",
	}
	for in, want := range cases {
		if got := normalizeSearchQuery(in); got != want {
			t.Errorf("normalizeSearchQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateSearchQuery(t *testing.T) {
	if err := validateSearchQuery("hello world"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := validateSearchQuery(strings.Repeat("a", maxSearchQueryBytes+1)); err == nil {
		t.Fatal("expected long query to be rejected")
	}
	if err := validateSearchQuery(strings.Repeat("a ", maxSearchTerms+1)); err == nil {
		t.Fatal("expected query with too many terms to be rejected")
	}
	if err := validateSearchQuery("bad\xff"); err == nil {
		t.Fatal("expected invalid UTF-8 to be rejected")
	}
}

func testSearchStore() *SearchStore {
	return &SearchStore{QueryLimit: 100, QueryIDsLimit: 500, QueryAuthorsLimit: 500, QueryKindsLimit: 10, QueryTagsLimit: 10}
}

func TestBuildSearchSQL(t *testing.T) {
	s := testSearchStore()

	q, params, ok, err := s.buildSearchSQL(nostr.Filter{Search: "hello", Kinds: []int{1, 1059}, Limit: 5}, false)
	if err != nil || !ok {
		t.Fatalf("unexpected result ok=%v err=%v", ok, err)
	}
	if !strings.Contains(q, searchVectorExpr+" @@ ") {
		t.Errorf("query does not use the indexed expression: %s", q)
	}
	if !strings.Contains(q, "kind <> 1059") {
		t.Errorf("query does not exclude gift wraps: %s", q)
	}
	if strings.Contains(q, "kind IN ($2,$3)") {
		t.Errorf("gift wrap kind was not stripped from kinds: %s", q)
	}
	order, limit := strings.Index(q, "ORDER BY ts_rank("), strings.Index(q, "LIMIT")
	if order < 0 || limit < order {
		t.Errorf("limit must be applied after relevance ordering: %s", q)
	}
	if params[len(params)-1] != 5 {
		t.Errorf("limit param = %v, want 5", params[len(params)-1])
	}

	_, params, _, _ = s.buildSearchSQL(nostr.Filter{Search: "hello", Limit: 100000}, false)
	if params[len(params)-1] != 100 {
		t.Errorf("limit not capped at QueryLimit: %v", params[len(params)-1])
	}

	if _, _, ok, err := s.buildSearchSQL(nostr.Filter{Search: "hello", Kinds: []int{1059}}, false); ok || err != nil {
		t.Errorf("gift-wrap-only search must not execute (ok=%v err=%v)", ok, err)
	}
	if _, _, ok, err := s.buildSearchSQL(nostr.Filter{Search: "include:spam"}, false); ok || err != nil {
		t.Errorf("extension-only search must not execute (ok=%v err=%v)", ok, err)
	}
	if _, _, _, err := s.buildSearchSQL(nostr.Filter{Search: "x", Tags: nostr.TagMap{"t": {}}}, false); err == nil {
		t.Error("expected empty tag set to be rejected")
	}
	if _, _, _, err := s.buildSearchSQL(nostr.Filter{Search: "x", Kinds: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}}, false); err == nil {
		t.Error("expected too many kinds to be rejected")
	}

	cq, _, ok, err := s.buildSearchSQL(nostr.Filter{Search: "hello"}, true)
	if err != nil || !ok || !strings.HasPrefix(cq, "SELECT COUNT(*)") || strings.Contains(cq, "LIMIT") {
		t.Errorf("unexpected count query: %s", cq)
	}
}

func TestSupportedNIPs(t *testing.T) {
	if !slices.Contains(supportedNIPs(true), any(50)) {
		t.Error("NIP-50 must be advertised when search is enabled")
	}
	if slices.Contains(supportedNIPs(false), any(50)) {
		t.Error("NIP-50 must not be advertised when search is disabled")
	}
	for _, n := range []int{1, 11, 17, 40, 42, 46, 70, 86} {
		if !slices.Contains(supportedNIPs(true), any(n)) {
			t.Errorf("NIP-%d missing", n)
		}
	}
}

// ---------------------------------------------------------------------------
// PostgreSQL integration tests
//
// Set SEARCH_TEST_DATABASE_URL to a PostgreSQL database to run them, e.g.
//   docker run --rm -d -p 127.0.0.1:55432:5432 -e POSTGRES_PASSWORD=postgres postgres:17
//   SEARCH_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable' go test ./...
// Each test works in its own temporary schema, which is dropped afterwards.
// ---------------------------------------------------------------------------

type searchEnv struct {
	backend *postgresql.PostgresBackend
	store   *SearchStore
	sqlDB   *sql.DB
	dbURL   string
}

func newSearchEnv(t *testing.T) *searchEnv {
	t.Helper()
	base := os.Getenv("SEARCH_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("SEARCH_TEST_DATABASE_URL not set")
	}

	admin, err := sql.Open("postgres", base)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("search_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		admin.Close()
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	qs := u.Query()
	qs.Set("search_path", schema)
	u.RawQuery = qs.Encode()
	dbURL := u.String()

	backend := &postgresql.PostgresBackend{DatabaseURL: dbURL, QueryLimit: 100}
	if err := backend.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backend.DB.Close() })

	sqlDB, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	return &searchEnv{
		backend: backend,
		sqlDB:   sqlDB,
		dbURL:   dbURL,
		store: &SearchStore{
			DB:                backend.DB,
			QueryLimit:        backend.QueryLimit,
			QueryIDsLimit:     backend.QueryIDsLimit,
			QueryAuthorsLimit: backend.QueryAuthorsLimit,
			QueryKindsLimit:   backend.QueryKindsLimit,
			QueryTagsLimit:    backend.QueryTagsLimit,
			Timeout:           5 * time.Second,
		},
	}
}

var idCounter int

func (e *searchEnv) add(t *testing.T, evt nostr.Event) nostr.Event {
	t.Helper()
	idCounter++
	if evt.ID == "" {
		evt.ID = fmt.Sprintf("%064x", idCounter)
	}
	if evt.PubKey == "" {
		evt.PubKey = strings.Repeat("a", 64)
	}
	if evt.Kind == 0 {
		evt.Kind = 1
	}
	if evt.CreatedAt == 0 {
		evt.CreatedAt = nostr.Timestamp(1700000000 + idCounter)
	}
	if evt.Tags == nil {
		evt.Tags = nostr.Tags{}
	}
	evt.Sig = strings.Repeat("0", 128)
	if err := e.backend.SaveEvent(context.Background(), &evt); err != nil {
		t.Fatal(err)
	}
	return evt
}

func (e *searchEnv) search(t *testing.T, f nostr.Filter) []string {
	t.Helper()
	ch, err := e.store.QueryEvents(context.Background(), f)
	if err != nil {
		t.Fatalf("search %+v: %v", f, err)
	}
	var contents []string
	for evt := range ch {
		contents = append(contents, evt.Content)
	}
	return contents
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func TestSearchIndexMigration(t *testing.T) {
	e := newSearchEnv(t)
	ctx := context.Background()

	// existing data before the index exists is backfilled by the build
	e.add(t, nostr.Event{Content: "pre-existing note about relays"})

	if err := EnsureSearchIndex(ctx, e.sqlDB); err != nil {
		t.Fatal(err)
	}
	// idempotent
	if err := EnsureSearchIndex(ctx, e.sqlDB); err != nil {
		t.Fatal(err)
	}

	var def string
	var valid bool
	err := e.sqlDB.QueryRow(`SELECT pg_get_indexdef(c.oid), i.indisvalid FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
		WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`, searchIndexName).Scan(&def, &valid)
	if err != nil {
		t.Fatal(err)
	}
	if !valid || !strings.Contains(def, "gin") || !strings.Contains(def, "1059") {
		t.Fatalf("unexpected index: valid=%v def=%s", valid, def)
	}

	// an INVALID index (interrupted concurrent build) is repaired
	if _, err := e.sqlDB.Exec(`UPDATE pg_index SET indisvalid = false WHERE indexrelid = $1::regclass`, searchIndexName); err != nil {
		t.Logf("cannot simulate invalid index (needs superuser): %v", err)
	} else {
		if err := EnsureSearchIndex(ctx, e.sqlDB); err != nil {
			t.Fatal(err)
		}
		if err := e.sqlDB.QueryRow(`SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass`, searchIndexName).Scan(&valid); err != nil || !valid {
			t.Fatalf("invalid index not rebuilt: valid=%v err=%v", valid, err)
		}
	}

	if got := e.search(t, nostr.Filter{Search: "relays"}); !sameSet(got, []string{"pre-existing note about relays"}) {
		t.Fatalf("backfilled event not found: %v", got)
	}

	// the planner can use the index for search queries
	q, params, _, err := e.store.buildSearchSQL(nostr.Filter{Search: "relays", Limit: 10}, false)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query("EXPLAIN "+q, params...)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		rows.Scan(&line)
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), searchIndexName) {
		t.Fatalf("search query does not use %s:\n%s", searchIndexName, plan.String())
	}
}

func TestSearchSemantics(t *testing.T) {
	e := newSearchEnv(t)
	if err := EnsureSearchIndex(context.Background(), e.sqlDB); err != nil {
		t.Fatal(err)
	}

	for _, c := range []string{
		"The quick brown fox jumps over the lazy dog",
		"A quick note about Nostr relays",
		"Hello, World! Punctuation-heavy: text; here.",
		"Ärger über die Straße",
		"東京都 is a city",
		"discount 100% off today",
		"snake_case identifiers and a*b math",
		"percent signs alone % here",
		"back\\slash content",
		"café au lait",
	} {
		e.add(t, nostr.Event{Content: c})
	}

	cases := []struct {
		name   string
		search string
		want   []string
	}{
		{"multi-word AND", "quick fox", []string{"The quick brown fox jumps over the lazy dog"}},
		{"multi-word any order", "fox quick", []string{"The quick brown fox jumps over the lazy dog"}},
		{"single word", "quick", []string{"The quick brown fox jumps over the lazy dog", "A quick note about Nostr relays"}},
		{"all words required", "quick elephant", nil},
		{"case-insensitive", "NOSTR RELAYS", []string{"A quick note about Nostr relays"}},
		{"punctuation in query", "hello, world!!!", []string{"Hello, World! Punctuation-heavy: text; here."}},
		{"hyphenated word", "punctuation-heavy", []string{"Hello, World! Punctuation-heavy: text; here."}},
		{"hyphen part", "heavy", []string{"Hello, World! Punctuation-heavy: text; here."}},
		{"unicode case folding", "ärger ÜBER", []string{"Ärger über die Straße"}},
		{"unicode sharp s", "straße", []string{"Ärger über die Straße"}},
		{"accents are significant", "cafe", nil},
		{"accented word", "CAFÉ", []string{"café au lait"}},
		{"CJK whole token", "東京都", []string{"東京都 is a city"}},
		{"CJK no substring", "東京", nil},
		{"whole words only", "quic", nil},
		{"no stemming", "relay", nil},
		{"literal percent is not a wildcard", "%", nil},
		{"percent attached to number", "100%", []string{"discount 100% off today"}},
		{"underscore is not a wildcard", "s_ake", nil},
		{"underscore splits words", "snake_case", []string{"snake_case identifiers and a*b math"}},
		{"asterisk is not a wildcard", "*", nil},
		{"asterisk is not prefix match", "qui*", nil},
		{"tsquery operators are literal", "quick & !fox | dog", []string{"The quick brown fox jumps over the lazy dog"}},
		{"backslash", "\\", nil},
		{"sql quotes", "'; DROP TABLE event; --", nil},
		{"extensions ignored", "relays include:spam language:en", []string{"A quick note about Nostr relays"}},
		{"extensions only", "include:spam", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.search(t, nostr.Filter{Search: c.search})
			if !sameSet(got, c.want) {
				t.Errorf("search %q = %q, want %q", c.search, got, c.want)
			}
		})
	}

	// the table must still be intact after the injection attempt
	if got := e.search(t, nostr.Filter{Search: "fox"}); len(got) != 1 {
		t.Fatalf("event table damaged: %v", got)
	}
}

func TestSearchCombinedFilters(t *testing.T) {
	e := newSearchEnv(t)
	alice, bob := strings.Repeat("a", 64), strings.Repeat("b", 64)

	e.add(t, nostr.Event{PubKey: alice, Kind: 1, CreatedAt: 1000, Content: "bitcoin alice note", Tags: nostr.Tags{{"t", "btc"}}})
	e.add(t, nostr.Event{PubKey: bob, Kind: 1, CreatedAt: 2000, Content: "bitcoin bob note", Tags: nostr.Tags{{"t", "btc"}}})
	e.add(t, nostr.Event{PubKey: alice, Kind: 30023, CreatedAt: 3000, Content: "bitcoin alice article", Tags: nostr.Tags{{"d", "x"}, {"t", "news"}}})
	e.add(t, nostr.Event{PubKey: bob, Kind: 1, CreatedAt: 4000, Content: "bitcoin bob other", Tags: nostr.Tags{{"e", "btc"}}})
	idTarget := e.add(t, nostr.Event{PubKey: bob, Kind: 1, CreatedAt: 5000, Content: "bitcoin by id"})
	e.add(t, nostr.Event{PubKey: alice, Kind: 1, CreatedAt: 6000, Content: "ethereum alice"})

	ts := func(v int) *nostr.Timestamp { x := nostr.Timestamp(v); return &x }
	cases := []struct {
		name string
		f    nostr.Filter
		want []string
	}{
		{"kinds", nostr.Filter{Search: "bitcoin", Kinds: []int{30023}}, []string{"bitcoin alice article"}},
		{"authors", nostr.Filter{Search: "bitcoin", Authors: []string{alice}}, []string{"bitcoin alice note", "bitcoin alice article"}},
		{"tag", nostr.Filter{Search: "bitcoin", Tags: nostr.TagMap{"t": {"btc"}}}, []string{"bitcoin alice note", "bitcoin bob note"}},
		{"tag name is respected", nostr.Filter{Search: "bitcoin", Tags: nostr.TagMap{"e": {"btc"}}}, []string{"bitcoin bob other"}},
		{"multiple tags AND", nostr.Filter{Search: "bitcoin", Tags: nostr.TagMap{"t": {"news"}, "d": {"x"}}}, []string{"bitcoin alice article"}},
		{"tag values OR", nostr.Filter{Search: "bitcoin", Tags: nostr.TagMap{"t": {"news", "btc"}}}, []string{"bitcoin alice note", "bitcoin bob note", "bitcoin alice article"}},
		{"since", nostr.Filter{Search: "bitcoin", Since: ts(3000)}, []string{"bitcoin alice article", "bitcoin bob other", "bitcoin by id"}},
		{"until", nostr.Filter{Search: "bitcoin", Until: ts(2000)}, []string{"bitcoin alice note", "bitcoin bob note"}},
		{"since+until", nostr.Filter{Search: "bitcoin", Since: ts(2000), Until: ts(3000)}, []string{"bitcoin bob note", "bitcoin alice article"}},
		{"ids", nostr.Filter{Search: "bitcoin", IDs: []string{idTarget.ID}}, []string{"bitcoin by id"}},
		{"ids no match", nostr.Filter{Search: "ethereum", IDs: []string{idTarget.ID}}, nil},
		{"all combined", nostr.Filter{Search: "bitcoin note", Kinds: []int{1}, Authors: []string{bob}, Tags: nostr.TagMap{"t": {"btc"}}, Since: ts(1500), Until: ts(2500)}, []string{"bitcoin bob note"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := e.search(t, c.f); !sameSet(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
			n, err := e.store.CountEvents(context.Background(), c.f)
			if err != nil || n != int64(len(c.want)) {
				t.Errorf("count = %d (err %v), want %d", n, err, len(c.want))
			}
		})
	}

	if got := e.search(t, nostr.Filter{Search: "bitcoin", Limit: 2}); len(got) != 2 {
		t.Errorf("limit not applied: %v", got)
	}
}

func TestSearchRelevanceBeforeLimit(t *testing.T) {
	e := newSearchEnv(t)

	// the most relevant event is the oldest; newer events mention the term
	// once inside much longer text
	best := e.add(t, nostr.Event{CreatedAt: 1000, Content: "zebra zebra zebra"})
	for i := 0; i < 20; i++ {
		e.add(t, nostr.Event{CreatedAt: nostr.Timestamp(2000 + i), Content: "a long note that mentions a zebra only once among many many other unrelated words " + strings.Repeat("filler ", 30)})
	}

	ch, err := e.store.QueryEvents(context.Background(), nostr.Filter{Search: "zebra", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got []*nostr.Event
	for evt := range ch {
		got = append(got, evt)
	}
	if len(got) != 1 || got[0].ID != best.ID {
		t.Fatalf("expected the most relevant (oldest) event first with limit 1, got %v", got)
	}

	// ties are broken by recency
	ch, _ = e.store.QueryEvents(context.Background(), nostr.Filter{Search: "zebra", Limit: 3})
	var times []nostr.Timestamp
	for evt := range ch {
		times = append(times, evt.CreatedAt)
	}
	if len(times) != 3 || times[0] != 1000 || times[1] != 2019 || times[2] != 2018 {
		t.Fatalf("unexpected order: %v", times)
	}
}

func TestSearchExcludesGiftWraps(t *testing.T) {
	e := newSearchEnv(t)
	recipient := strings.Repeat("c", 64)
	e.add(t, nostr.Event{Kind: 1059, Content: "secret plaintext marker", Tags: nostr.Tags{{"p", recipient}}})
	e.add(t, nostr.Event{Kind: 1, Content: "public plaintext marker"})

	for _, f := range []nostr.Filter{
		{Search: "marker"},
		{Search: "marker", Kinds: []int{1059}},
		{Search: "marker", Kinds: []int{1, 1059}},
		{Search: "marker", Tags: nostr.TagMap{"p": {recipient}}},
		{Search: "marker", Kinds: []int{1059}, Tags: nostr.TagMap{"p": {recipient}}},
	} {
		got := e.search(t, f)
		for _, c := range got {
			if strings.Contains(c, "secret") {
				t.Errorf("gift wrap returned for filter %+v", f)
			}
		}
		n, err := e.store.CountEvents(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		if n != int64(len(got)) {
			t.Errorf("count %d != results %d for %+v", n, len(got), f)
		}
	}
}

// TestSearchRelayEndToEnd runs the real khatru routing over a websocket.
func TestSearchRelayEndToEnd(t *testing.T) {
	e := newSearchEnv(t)

	router := khatru.NewRouter()
	relay := router.Relay
	relay.StoreEvent = append(relay.StoreEvent, e.backend.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, e.backend.QueryEvents)
	relay.CountEvents = append(relay.CountEvents, e.backend.CountEvents)
	relay.RejectFilter = append(relay.RejectFilter, RejectNonAuthenticatedGiftWrapQueries)
	router.Route().Req(isSearchFilter).Relay(newSearchRelay(relay, e.store))
	// added after the route: must still be enforced for searches
	relay.RejectFilter = append(relay.RejectFilter, func(ctx context.Context, f nostr.Filter) (bool, string) {
		if slices.Contains(f.Authors, strings.Repeat("f", 64)) {
			return true, "blocked: test policy"
		}
		return false, ""
	})

	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := nostr.RelayConnect(ctx, "ws"+server.URL[4:])
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sk := nostr.GeneratePrivateKey()
	publish := func(kind int, content string, tags nostr.Tags) nostr.Event {
		evt := nostr.Event{Kind: kind, CreatedAt: nostr.Now(), Content: content, Tags: tags}
		if err := evt.Sign(sk); err != nil {
			t.Fatal(err)
		}
		if err := conn.Publish(ctx, evt); err != nil {
			t.Fatal(err)
		}
		return evt
	}
	publish(1, "giraffe giraffe giraffe", nil)
	publish(1, "a single giraffe in a much longer sentence with lots of words", nil)
	publish(1059, "giraffe ciphertext", nostr.Tags{{"p", strings.Repeat("c", 64)}})

	// stored results arrive ranked, without gift wraps
	sub, err := conn.Subscribe(ctx, nostr.Filters{{Search: "giraffe", Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
collect:
	for {
		select {
		case evt := <-sub.Events:
			got = append(got, evt.Content)
		case <-sub.EndOfStoredEvents:
			break collect
		case <-ctx.Done():
			t.Fatal("timeout waiting for EOSE")
		}
	}
	if len(got) != 2 || got[0] != "giraffe giraffe giraffe" {
		t.Fatalf("unexpected search results: %q", got)
	}

	// search subscriptions are stored-only: no live events after EOSE, even
	// ones that match the other constraints (or the search itself)
	normal, err := conn.Subscribe(ctx, nostr.Filters{{Kinds: []int{1}, Limit: 0, LimitZero: true}})
	if err != nil {
		t.Fatal(err)
	}
	publish(1, "unrelated live note", nil)
	publish(1, "live giraffe", nil)
	received := 0
	timeout := time.After(1500 * time.Millisecond)
wait:
	for {
		select {
		case evt := <-sub.Events:
			t.Fatalf("search subscription received live event %q", evt.Content)
		case <-normal.Events:
			received++
		case <-timeout:
			break wait
		}
	}
	if received != 2 {
		t.Fatalf("normal subscription received %d live events, want 2", received)
	}

	// main relay filter policies are applied to searches
	closed, err := conn.Subscribe(ctx, nostr.Filters{{Search: "giraffe", Kinds: []int{1059}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-closed.ClosedReason:
		if !strings.HasPrefix(reason, "auth-required:") {
			t.Fatalf("unexpected CLOSED reason %q", reason)
		}
	case <-closed.EndOfStoredEvents:
		t.Fatal("unauthenticated gift wrap search was not rejected")
	case <-ctx.Done():
		t.Fatal("timeout")
	}

	blocked, err := conn.Subscribe(ctx, nostr.Filters{{Search: "giraffe", Authors: []string{strings.Repeat("f", 64)}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-blocked.ClosedReason:
		if !strings.Contains(reason, "test policy") {
			t.Fatalf("unexpected CLOSED reason %q", reason)
		}
	case <-blocked.EndOfStoredEvents:
		t.Fatal("policy added after routing was not applied to search")
	case <-ctx.Done():
		t.Fatal("timeout")
	}

	tooLong, err := conn.Subscribe(ctx, nostr.Filters{{Search: strings.Repeat("x ", maxSearchTerms+1)}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-tooLong.ClosedReason:
		if !strings.HasPrefix(reason, "invalid:") {
			t.Fatalf("unexpected CLOSED reason %q", reason)
		}
	case <-tooLong.EndOfStoredEvents:
		t.Fatal("oversized search was not rejected")
	case <-ctx.Done():
		t.Fatal("timeout")
	}

	// COUNT is routed to the search store too
	n, _, err := conn.Count(ctx, nostr.Filters{{Search: "giraffe"}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // two stored + "live giraffe"
		t.Fatalf("count = %d, want 3", n)
	}
}
