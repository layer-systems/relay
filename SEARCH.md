# NIP-50 search

The relay implements [NIP-50](https://github.com/nostr-protocol/nips/blob/master/50.md)
on top of PostgreSQL full-text search and announces `50` in its NIP-11
`supported_nips` while search is enabled.

```json
["REQ", "sub", {"search": "nostr relays", "kinds": [1], "limit": 20}]
```

## Query semantics

| Aspect | Behaviour |
| --- | --- |
| Parser | `plainto_tsquery('simple', <search>)` |
| Terms | Whitespace/punctuation separated words. **All** words must appear (AND). Word order and adjacency do not matter. |
| Case | Case-insensitive, including non-ASCII letters (`ÄRGER` matches `ärger`). |
| Word matching | Whole words only. No prefix, substring or fuzzy matching (`quic` does not match `quick`). |
| Language | The `simple` configuration: no stemming (`relay` does not match `relays`), no stop words, no accent folding (`cafe` does not match `café`). It treats every language the same, which suits a multilingual relay. |
| Punctuation | Treated as a separator. `hello, world!` searches for `hello` and `world`; `punctuation-heavy` matches both the compound and its parts. |
| Operators / wildcards | None. `& \| ! : * ( ) ' " % _ \` are literal input and never act as boolean operators, wildcards or SQL; mostly they are dropped as punctuation. A search made only of such characters matches nothing. |
| Scripts without spaces | CJK and similar text is indexed as one token per unbroken run, so `東京` does not match `東京都`. |
| NIP-50 extensions | `include:`, `domain:`, `language:`, `sentiment:` and `nsfw:` tokens are recognised and ignored, as NIP-50 asks. A search that contains only extensions returns no events. Other `key:value` text is searched as words. |
| Fields | Event `content` only (first 65,536 characters). Tags are not full-text searched. |
| Ordering | By relevance: `ts_rank(..., 1)` (term frequency normalised by document length), then `created_at DESC`, then `id`. The `limit` is applied **after** ranking, as NIP-50 recommends. |
| Other filter fields | `ids`, `authors`, `kinds`, `#<tag>`, `since`, `until` and `limit` are applied exactly as in normal queries, with the same limits (500 ids, 500 authors, 10 kinds, 10 tag values, `limit` capped at `QUERY_LIMIT`). Tag filters also respect the tag name (`#e` does not match a `t` tag). |
| Live events | Search subscriptions return stored events only. After `EOSE` no further events are sent on them; clients that want new results should repeat the search. |
| COUNT (NIP-45) | Supported with the same matching rules. |
| Input limits | At most 512 bytes and 32 words. Larger searches are rejected with `CLOSED` `invalid: ...`. |

## Privacy: NIP-17 gift wraps

Search **never** returns kind `1059` gift wraps, whether or not the client is
authenticated and whatever `kinds`/`#p` the filter has. The SQL always
includes `kind <> 1059`, which is also the predicate of the partial search
index, so gift wraps are not even indexed. Their content is ciphertext, so
nothing useful is lost. To read gift wraps, query them without `search`, using
the normal authenticated NIP-17 path.

All filter policies of the main relay (rate limits, `NoComplexFilters`, the
NIP-17 filter rule) also run for search requests. They run at request
time, so policies added to `relay.RejectFilter`, `RejectCountFilter` or
`OverwriteFilter` later are applied too.

## Implementation

* `search.go` builds its own SQL for search filters. Everything else still goes
  through `eventstore/postgresql`.
* Search filters go to a dedicated khatru sub-relay (`khatru.Router`). This
  keeps live events away from search subscriptions (go-nostr's
  `Filter.Matches` ignores `search`, so the main relay would otherwise stream
  every new event that matches the other constraints).
* Index:

  ```sql
  CREATE INDEX CONCURRENTLY IF NOT EXISTS event_content_fts_simple_idx
    ON event USING gin ((to_tsvector('simple'::regconfig, left(content, 65536))))
    WHERE kind <> 1059;
  ```

  This is an expression index, not a stored column, so the `event` table does
  not need to be rewritten. `left(content, 65536)` keeps every tsvector far
  below PostgreSQL's 1 MB limit, so indexing can never make an `INSERT` fail.

### Why not upgrade to eventstore v0.17.x?

eventstore v0.17.15 has built-in full-text search (`plainto_tsquery`,
`FullTextSearchConfig`, `SubstringSearch`). It was reviewed and not adopted
here, because:

* it orders search results by `created_at`, not relevance, so the limit is not
  applied after ranking;
* it changes the `tagvalues` generated column format to `"<name>:<value>"`.
  `CREATE TABLE IF NOT EXISTS` does not migrate existing tables, so tag
  queries against an existing database would break without a separate data
  migration;
* it needs Go ≥ 1.25 (`go.mod` currently says 1.24.4);
* it does not create a full-text index.

A future upgrade can still swap in the library's matching, provided ranking and
the gift-wrap exclusion are kept.

## Operations

### Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `SEARCH_ENABLED` | `true` | `false` turns search off: search filters are rejected with `unsupported:` and NIP-50 is no longer advertised. |
| `SEARCH_INDEX_BUILD` | `background` | `background`: create or repair the index in a goroutine at startup (the relay serves immediately, and searches seq-scan until the index is ready). `sync`: build before serving (startup fails on error). `off`: the operator manages the index. |
| `SEARCH_TIMEOUT` | `5s` | Upper bound for one search statement (Go duration). On timeout the client gets a `NOTICE` and `EOSE`. |

### Migration and backfill of existing data

No data changes are needed. Building the index **is** the backfill: PostgreSQL
indexes every existing row while it builds.

* The build uses `CREATE INDEX CONCURRENTLY`, so reads and writes to `event`
  keep working (only schema changes and `VACUUM FULL` are blocked). It does two
  table scans and waits for transactions already running.
* A PostgreSQL advisory lock means only one relay instance builds at a time.
* An interrupted build leaves an `INVALID` index. On the next start it is
  detected, dropped (`DROP INDEX CONCURRENTLY`) and rebuilt.
* On large databases you can build ahead of the deploy with the SQL above
  (from `psql`, outside a transaction), raising `maintenance_work_mem` for
  that session to speed it up, and then run with `SEARCH_INDEX_BUILD=off`.
* Rollback: `DROP INDEX CONCURRENTLY event_content_fts_simple_idx;`. Nothing
  else in the schema is touched.

Progress: `SELECT * FROM pg_stat_progress_create_index;`. Validity:
`SELECT indisvalid FROM pg_index WHERE indexrelid = 'event_content_fts_simple_idx'::regclass;`.

### Cost

Measured on PostgreSQL 17, 200k synthetic notes (~300 bytes each):

* Index size about 13 MB against a 59 MB table (~20–25%). Build time about 3 s.
* Every insert/update now also tokenises the content into the GIN index
  (GIN's pending list batches this; autovacuum folds it in).
* Rare term (4 matches): about 0.1 ms.
* Very common term (40k matches): about 0.5 s. Ranking needs the tsvector of
  every matching row, and GIN does not store it, so ranking recomputes it.
  Cost grows linearly with the number of matches, not the limit. Narrower
  filters (`kinds`, `authors`, `since`) and more specific searches help;
  `SEARCH_TIMEOUT` caps the worst case. If this becomes a problem, the next
  step is a stored `tsvector` column (one-time table rewrite) so ranking reads
  it instead of recomputing it.

### Limitations

* No phrase, prefix, boolean or fuzzy syntax; no stemming or accent folding.
* No word segmentation for CJK and other scripts written without spaces.
* Only the first 65,536 characters of content are searchable.
* Results are not live (stored events only).
* Changing the text search configuration means changing `searchTSConfig`,
  dropping the index and rebuilding it.

### Tests

```sh
docker run --rm -d --name relay-search-pg -p 127.0.0.1:55432:5432 \
  -e POSTGRES_PASSWORD=postgres postgres:17
SEARCH_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable' \
  go test ./...
```

Without `SEARCH_TEST_DATABASE_URL`, only the unit tests run. Each integration
test uses a temporary schema that is dropped afterwards.
