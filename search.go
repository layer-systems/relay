package main

// NIP-50 search backed by PostgreSQL full-text search.
//
// See SEARCH.md for the user-facing semantics and the operational notes. In
// short:
//
//   - The search string is parsed with plainto_tsquery using the language
//     neutral "simple" text search configuration: every word must be present
//     (AND), matching is case-insensitive on whole words, punctuation and
//     operator characters are treated as separators, and nothing is stemmed.
//   - Results are ordered by relevance (ts_rank) and the limit is applied
//     after ranking, as recommended by NIP-50.
//   - Every normal filter constraint (ids, authors, kinds, tags, since, until,
//     limit) is applied in the same SQL statement.
//   - Kind 1059 gift wraps (NIP-17) are never returned by a search, regardless
//     of authentication. Their content is ciphertext, so there is nothing
//     useful to search, and excluding them means search cannot become a side
//     channel around recipient protection.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fiatjaf/khatru"
	"github.com/jmoiron/sqlx"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip45/hyperloglog"
)

const (
	// searchTSConfig is the PostgreSQL text search configuration. It is part
	// of the index expression, so changing it requires rebuilding the index.
	searchTSConfig = "simple"

	// searchMaxContentChars bounds how much of an event's content is indexed.
	// It keeps index entries small and guarantees to_tsvector can never fail
	// on insert (a tsvector is limited to 1MB), which would otherwise make
	// the INSERT of a very large event fail.
	searchMaxContentChars = 65536

	// searchIndexName is the name of the partial GIN expression index.
	searchIndexName = "event_content_fts_simple_idx"

	// giftWrapKind is the NIP-59/NIP-17 gift wrap kind excluded from search.
	giftWrapKind = 1059

	// maxSearchQueryBytes and maxSearchTerms bound the work a single search
	// string can cause.
	maxSearchQueryBytes = 512
	maxSearchTerms      = 32

	// searchAdvisoryLockKey serialises index builds across relay instances
	// sharing one database.
	searchAdvisoryLockKey int64 = 0x6e697035305f6674 // "nip50_ft"
)

// searchVectorExpr must stay byte-for-byte identical between the index
// definition and the queries, otherwise PostgreSQL cannot use the index.
var searchVectorExpr = fmt.Sprintf("to_tsvector('%s'::regconfig, left(content, %d))", searchTSConfig, searchMaxContentChars)

// searchExclusionPredicate is the partial-index predicate. It is embedded as a
// literal (not a bind parameter) in every search query so the planner can
// prove the partial index applies.
var searchExclusionPredicate = fmt.Sprintf("kind <> %d", giftWrapKind)

var (
	errSearchTooManyIDs       = errors.New("invalid: too many ids")
	errSearchTooManyAuthors   = errors.New("invalid: too many authors")
	errSearchTooManyKinds     = errors.New("invalid: too many kinds")
	errSearchTooManyTagValues = errors.New("invalid: too many tag values")
	errSearchEmptyTagSet      = errors.New("invalid: empty tag set")
)

// NIP-50 defines optional "key:value" extensions. This relay does not
// implement any of them, so (as the NIP asks) they are ignored instead of
// being searched for as literal words.
var nip50ExtensionToken = regexp.MustCompile(`(?i)^(include|domain|language|sentiment|nsfw):\S*$`)

// normalizeSearchQuery removes unsupported NIP-50 extensions and characters
// PostgreSQL cannot accept in text values. An empty result means the search has
// no terms left to match.
func normalizeSearchQuery(q string) string {
	q = strings.ToValidUTF8(q, " ")
	q = strings.ReplaceAll(q, "\x00", " ")
	fields := strings.Fields(q)
	kept := fields[:0]
	for _, f := range fields {
		if nip50ExtensionToken.MatchString(f) {
			continue
		}
		kept = append(kept, f)
	}
	return strings.Join(kept, " ")
}

// validateSearchQuery rejects search strings that are too expensive to run.
func validateSearchQuery(q string) error {
	if len(q) > maxSearchQueryBytes {
		return fmt.Errorf("invalid: search query longer than %d bytes", maxSearchQueryBytes)
	}
	if !utf8.ValidString(q) {
		return errors.New("invalid: search query is not valid UTF-8")
	}
	if n := len(strings.Fields(q)); n > maxSearchTerms {
		return fmt.Errorf("invalid: search query has more than %d terms", maxSearchTerms)
	}
	return nil
}

// SearchStore runs NIP-50 queries against the eventstore PostgreSQL schema.
type SearchStore struct {
	DB *sqlx.DB

	QueryLimit        int
	QueryIDsLimit     int
	QueryAuthorsLimit int
	QueryKindsLimit   int
	QueryTagsLimit    int

	// Timeout bounds a single search statement (0 = no extra bound).
	Timeout time.Duration
}

// buildSearchSQL builds the search statement. It returns ok=false when the
// filter can never match anything (no terms after normalisation, or only kind
// 1059 requested), in which case no query should be executed.
func (s *SearchStore) buildSearchSQL(filter nostr.Filter, doCount bool) (query string, params []any, ok bool, err error) {
	search := normalizeSearchQuery(filter.Search)
	if search == "" {
		return "", nil, false, nil
	}

	params = make([]any, 0, 16)
	arg := func(v any) string {
		params = append(params, v)
		return "$" + strconv.Itoa(len(params))
	}
	argList := func(n int, get func(i int) any) string {
		ph := make([]string, n)
		for i := 0; i < n; i++ {
			ph[i] = arg(get(i))
		}
		return strings.Join(ph, ",")
	}

	tsquery := fmt.Sprintf("plainto_tsquery('%s'::regconfig, %s)", searchTSConfig, arg(search))
	conditions := []string{
		searchVectorExpr + " @@ " + tsquery,
		searchExclusionPredicate,
	}

	if len(filter.IDs) > 0 {
		if len(filter.IDs) > s.QueryIDsLimit {
			return "", nil, false, errSearchTooManyIDs
		}
		conditions = append(conditions, "id IN ("+argList(len(filter.IDs), func(i int) any { return filter.IDs[i] })+")")
	}

	if len(filter.Authors) > 0 {
		if len(filter.Authors) > s.QueryAuthorsLimit {
			return "", nil, false, errSearchTooManyAuthors
		}
		conditions = append(conditions, "pubkey IN ("+argList(len(filter.Authors), func(i int) any { return filter.Authors[i] })+")")
	}

	if len(filter.Kinds) > 0 {
		kinds := make([]int, 0, len(filter.Kinds))
		for _, k := range filter.Kinds {
			if k != giftWrapKind {
				kinds = append(kinds, k)
			}
		}
		if len(kinds) == 0 {
			// only gift wraps were requested; search never returns those
			return "", nil, false, nil
		}
		if len(filter.Kinds) > s.QueryKindsLimit {
			return "", nil, false, errSearchTooManyKinds
		}
		conditions = append(conditions, "kind IN ("+argList(len(kinds), func(i int) any { return kinds[i] })+")")
	}

	totalTags := 0
	for name, values := range filter.Tags {
		if len(values) == 0 {
			return "", nil, false, errSearchEmptyTagSet
		}
		totalTags += len(values)
		if totalTags > s.QueryTagsLimit {
			return "", nil, false, errSearchTooManyTagValues
		}
		vals := argList(len(values), func(i int) any { return values[i] })
		// tagvalues (GIN-indexed) narrows candidates; the jsonb check makes
		// the tag name part of the match, exactly like nostr.Filter.Matches.
		conditions = append(conditions, fmt.Sprintf(
			"tagvalues && ARRAY[%s]::text[] AND EXISTS (SELECT 1 FROM jsonb_array_elements(tags) AS t WHERE t->>0 = %s AND t->>1 IN (%s))",
			vals, arg(name), vals))
	}

	if filter.Since != nil {
		conditions = append(conditions, "created_at >= "+arg(int64(*filter.Since)))
	}
	if filter.Until != nil {
		conditions = append(conditions, "created_at <= "+arg(int64(*filter.Until)))
	}

	where := strings.Join(conditions, " AND ")
	if doCount {
		return "SELECT COUNT(*) FROM event WHERE " + where, params, true, nil
	}

	limit := filter.Limit
	if limit < 1 || limit > s.QueryLimit {
		limit = s.QueryLimit
	}

	// rank normalisation 1 divides by 1 + log(document length) so long
	// keyword-stuffed notes do not automatically outrank short exact ones.
	query = "SELECT id, pubkey, created_at, kind, tags, content, sig FROM event WHERE " + where +
		" ORDER BY ts_rank(" + searchVectorExpr + ", " + tsquery + ", 1) DESC, created_at DESC, id" +
		" LIMIT " + arg(limit)
	return query, params, true, nil
}

func (s *SearchStore) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.Timeout > 0 {
		return context.WithTimeout(ctx, s.Timeout)
	}
	return context.WithCancel(ctx)
}

// QueryEvents returns search results ordered by relevance. Filters without a
// search string yield nothing: this store only serves NIP-50 queries.
func (s *SearchStore) QueryEvents(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
	ch := make(chan *nostr.Event)
	if filter.Search == "" {
		close(ch)
		return ch, nil
	}

	query, params, ok, err := s.buildSearchSQL(filter, false)
	if err != nil {
		return nil, err
	}
	if !ok {
		close(ch)
		return ch, nil
	}

	qctx, cancel := s.withTimeout(ctx)
	rows, err := s.DB.QueryContext(qctx, query, params...)
	if err != nil {
		cancel()
		if errors.Is(qctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("error: search timed out, try a more specific query")
		}
		return nil, fmt.Errorf("error: search failed: %w", err)
	}

	go func() {
		defer cancel()
		defer rows.Close()
		defer close(ch)
		for rows.Next() {
			var evt nostr.Event
			var createdAt int64
			if err := rows.Scan(&evt.ID, &evt.PubKey, &createdAt, &evt.Kind, &evt.Tags, &evt.Content, &evt.Sig); err != nil {
				log.Printf("search: scan failed: %v", err)
				return
			}
			evt.CreatedAt = nostr.Timestamp(createdAt)
			select {
			case ch <- &evt:
			case <-ctx.Done():
				return
			}
		}
		if err := rows.Err(); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("search: query failed: %v", err)
		}
	}()
	return ch, nil
}

// CountEvents implements NIP-45 COUNT for search filters.
func (s *SearchStore) CountEvents(ctx context.Context, filter nostr.Filter) (int64, error) {
	if filter.Search == "" {
		return 0, nil
	}
	query, params, ok, err := s.buildSearchSQL(filter, true)
	if err != nil || !ok {
		return 0, err
	}
	qctx, cancel := s.withTimeout(ctx)
	defer cancel()
	var count int64
	if err := s.DB.QueryRowContext(qctx, query, params...).Scan(&count); err != nil {
		return 0, fmt.Errorf("error: search count failed: %w", err)
	}
	return count, nil
}

// searchIndexDDL returns the statement that creates the search index. It is
// exported through SEARCH.md as well so operators can run it by hand.
func searchIndexDDL() string {
	return fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON event USING gin ((%s)) WHERE %s",
		searchIndexName, searchVectorExpr, searchExclusionPredicate)
}

// EnsureSearchIndex creates (or repairs) the full-text index. Building it is
// also the backfill for existing events. CONCURRENTLY keeps the event table
// writable during the build; an interrupted build leaves an INVALID index,
// which is dropped and rebuilt here.
func EnsureSearchIndex(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", searchAdvisoryLockKey).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		log.Printf("search: another instance is building %s, skipping", searchIndexName)
		return nil
	}
	defer conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", searchAdvisoryLockKey)

	var valid sql.NullBool
	err = conn.QueryRowContext(ctx, `
		SELECT i.indisvalid
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`, searchIndexName).Scan(&valid)
	switch {
	case err == nil && valid.Bool:
		return nil
	case err == nil:
		log.Printf("search: dropping invalid index %s left by an interrupted build", searchIndexName)
		if _, err := conn.ExecContext(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+searchIndexName); err != nil {
			return err
		}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}

	start := time.Now()
	log.Printf("search: building %s (backfills existing events, table stays writable)", searchIndexName)
	if _, err := conn.ExecContext(ctx, searchIndexDDL()); err != nil {
		return err
	}
	log.Printf("search: %s ready after %s", searchIndexName, time.Since(start).Round(time.Millisecond))
	return nil
}

// isSearchFilter routes NIP-50 filters to the search sub-relay.
func isSearchFilter(f nostr.Filter) bool { return f.Search != "" }

// rejectInvalidSearch is a RejectFilter for search filters.
func rejectInvalidSearch(ctx context.Context, filter nostr.Filter) (bool, string) {
	if err := validateSearchQuery(filter.Search); err != nil {
		return true, err.Error()
	}
	return false, ""
}

// rejectSearchDisabled is used instead of the search relay when SEARCH_ENABLED=false.
func rejectSearchDisabled(ctx context.Context, filter nostr.Filter) (bool, string) {
	if filter.Search != "" {
		return true, "unsupported: NIP-50 search is disabled on this relay"
	}
	return false, ""
}

// newSearchRelay builds the sub-relay that serves search filters.
//
// Search filters are routed here (see khatru.Router) for two reasons:
//
//  1. go-nostr's Filter.Matches ignores "search", so a live subscription on
//     the main relay would stream every new event matching the other
//     constraints. No events are ever routed to this sub-relay, so its
//     listeners never receive live events: search subscriptions are
//     stored-events-only and go quiet after EOSE.
//  2. The search query path is separate from the normal store path.
//
// All filter policies of the main relay (rate limits, complexity limits,
// NIP-17 rules) are evaluated at request time, so policies appended to the main
// relay later still apply to searches.
func newSearchRelay(main *khatru.Relay, store *SearchStore) *khatru.Relay {
	sr := khatru.NewRelay()
	sr.Info = main.Info
	sr.OverwriteFilter = append(sr.OverwriteFilter, func(ctx context.Context, f *nostr.Filter) {
		for _, ovw := range main.OverwriteFilter {
			ovw(ctx, f)
		}
	})
	delegate := func(rejects *[]func(context.Context, nostr.Filter) (bool, string)) func(context.Context, nostr.Filter) (bool, string) {
		return func(ctx context.Context, f nostr.Filter) (bool, string) {
			for _, reject := range *rejects {
				if r, msg := reject(ctx, f); r {
					return true, msg
				}
			}
			return false, ""
		}
	}
	sr.RejectFilter = append(sr.RejectFilter, rejectInvalidSearch, delegate(&main.RejectFilter))
	sr.RejectCountFilter = append(sr.RejectCountFilter, rejectInvalidSearch, delegate(&main.RejectCountFilter))
	sr.QueryEvents = append(sr.QueryEvents, store.QueryEvents)
	sr.CountEvents = append(sr.CountEvents, store.CountEvents)
	sr.CountEventsHLL = append(sr.CountEventsHLL, func(ctx context.Context, f nostr.Filter, offset int) (int64, *hyperloglog.HyperLogLog, error) {
		n, err := store.CountEvents(ctx, f)
		return n, nil, err
	})
	return sr
}
