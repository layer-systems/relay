package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/jmoiron/sqlx"
	"github.com/nbd-wtf/go-nostr"
)

func queryEventsWithSearch(db *postgresql.PostgresBackend) func(context.Context, nostr.Filter) (chan *nostr.Event, error) {
	return func(ctx context.Context, filter nostr.Filter) (chan *nostr.Event, error) {
		if strings.TrimSpace(filter.Search) == "" {
			return db.QueryEvents(ctx, filter)
		}

		query, params, err := searchEventsSQL(db, filter, false)
		if err != nil {
			return nil, err
		}

		rows, err := db.DB.QueryContext(ctx, query, params...)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("failed to fetch search results using query %q: %w", query, err)
		}

		ch := make(chan *nostr.Event)
		go func() {
			defer rows.Close()
			defer close(ch)

			for rows.Next() {
				var evt nostr.Event
				var timestamp int64
				if err := rows.Scan(&evt.ID, &evt.PubKey, &timestamp, &evt.Kind, &evt.Tags, &evt.Content, &evt.Sig); err != nil {
					return
				}
				evt.CreatedAt = nostr.Timestamp(timestamp)

				select {
				case ch <- &evt:
				case <-ctx.Done():
					return
				}
			}
		}()

		return ch, nil
	}
}

func countEventsWithSearch(db *postgresql.PostgresBackend) func(context.Context, nostr.Filter) (int64, error) {
	return func(ctx context.Context, filter nostr.Filter) (int64, error) {
		if strings.TrimSpace(filter.Search) == "" {
			return db.CountEvents(ctx, filter)
		}

		query, params, err := searchEventsSQL(db, filter, true)
		if err != nil {
			return 0, err
		}

		var count int64
		if err = db.DB.QueryRowContext(ctx, query, params...).Scan(&count); err != nil && err != sql.ErrNoRows {
			return 0, fmt.Errorf("failed to count search results using query %q: %w", query, err)
		}
		return count, nil
	}
}

func searchEventsSQL(db *postgresql.PostgresBackend, filter nostr.Filter, doCount bool) (string, []any, error) {
	conditions := make([]string, 0, 8)
	params := make([]any, 0, 32)

	if len(filter.IDs) > 0 {
		if len(filter.IDs) > db.QueryIDsLimit {
			return "", nil, postgresql.TooManyIDs
		}
		for _, id := range filter.IDs {
			params = append(params, id)
		}
		conditions = append(conditions, `id IN (`+placeholders(len(filter.IDs))+`)`)
	}

	if len(filter.Authors) > 0 {
		if len(filter.Authors) > db.QueryAuthorsLimit {
			return "", nil, postgresql.TooManyAuthors
		}
		for _, author := range filter.Authors {
			params = append(params, author)
		}
		conditions = append(conditions, `pubkey IN (`+placeholders(len(filter.Authors))+`)`)
	}

	if len(filter.Kinds) > 0 {
		if len(filter.Kinds) > db.QueryKindsLimit {
			return "", nil, postgresql.TooManyKinds
		}
		for _, kind := range filter.Kinds {
			params = append(params, kind)
		}
		conditions = append(conditions, `kind IN (`+placeholders(len(filter.Kinds))+`)`)
	}

	totalTags := 0
	for _, values := range filter.Tags {
		if len(values) == 0 {
			return "", nil, postgresql.EmptyTagSet
		}
		for _, tagValue := range values {
			params = append(params, tagValue)
		}
		conditions = append(conditions, `tagvalues && ARRAY[`+placeholders(len(values))+`]`)

		totalTags += len(values)
		if totalTags > db.QueryTagsLimit {
			return "", nil, postgresql.TooManyTagValues
		}
	}

	if filter.Since != nil {
		conditions = append(conditions, `created_at >= ?`)
		params = append(params, int64(*filter.Since))
	}
	if filter.Until != nil {
		conditions = append(conditions, `created_at <= ?`)
		params = append(params, int64(*filter.Until))
	}

	phrase, terms := normalizeSearchQuery(filter.Search)
	searchConditions := make([]string, 0, len(terms)+1)
	scoreParts := make([]string, 0, len(terms)+1)
	if phrase != "" {
		searchConditions = append(searchConditions, `content ILIKE ? ESCAPE '\'`)
		scoreParts = append(scoreParts, `CASE WHEN content ILIKE ? ESCAPE '\' THEN 100 ELSE 0 END`)
		params = append(params, likePattern(phrase))
	}
	for _, term := range terms {
		searchConditions = append(searchConditions, `content ILIKE ? ESCAPE '\'`)
		scoreParts = append(scoreParts, `CASE WHEN content ILIKE ? ESCAPE '\' THEN 1 ELSE 0 END`)
		params = append(params, likePattern(term))
	}
	if len(searchConditions) > 0 {
		conditions = append(conditions, `(`+strings.Join(searchConditions, " OR ")+`)`)
	}

	if len(conditions) == 0 {
		conditions = append(conditions, `true`)
	}

	limit := db.QueryLimit
	if filter.Limit > 0 && filter.Limit <= db.QueryLimit {
		limit = filter.Limit
	}

	var query string
	if doCount {
		query = `SELECT COUNT(*) FROM event WHERE ` + strings.Join(conditions, " AND ")
	} else {
		score := "0"
		if len(scoreParts) > 0 {
			score = strings.Join(scoreParts, " + ")
			if phrase != "" {
				params = append(params, likePattern(phrase))
			}
			for _, term := range terms {
				params = append(params, likePattern(term))
			}
		}
		params = append(params, limit)
		query = `SELECT id, pubkey, created_at, kind, tags, content, sig FROM event WHERE ` +
			strings.Join(conditions, " AND ") +
			` ORDER BY (` + score + `) DESC, created_at DESC, id LIMIT ?`
	}

	return sqlx.Rebind(sqlx.BindType("postgres"), query), params, nil
}

func normalizeSearchQuery(search string) (string, []string) {
	fields := strings.Fields(search)
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		if strings.Contains(field, ":") {
			continue
		}
		terms = append(terms, field)
	}

	return strings.Join(terms, " "), terms
}

func likePattern(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + replacer.Replace(value) + "%"
}

func placeholders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}
