# Successor stack migration

The relay uses fiatjaf.com/nostr at commit 96f89895a4b2 (Go 1.25).
Docker already builds with Go 1.25.5; CI reads go.mod.

The successor module has no PostgreSQL store. storage.go isolates the original
PostgreSQL backend behind a JSON wire-format adapter. This intentionally retains
eventstore and go-nostr dependencies for persistence and interoperability tests.
The archived Khatru dependency is removed. No event or management table is
rewritten; DATABASE_URL, QUERY_LIMIT and deployment entrypoints are unchanged.

Before deployment, back up PostgreSQL and test against a restored copy. Deploy by
rebuilding the existing Compose service. Roll back by redeploying the previous
image; no reverse schema migration is required. The PostgreSQL integration test
reopens a store with existing events and runs gift-wrap tests against PostgreSQL.
TEST_DATABASE_URL must point to an isolated test database: tests truncate event.

Successor hooks are single functions, queries are iterators, keys are fixed-size
values and connections can authenticate multiple public keys. Gift-wrap checks
cover stored and live delivery; COUNT uses recipient restrictions. Ban lookup
failures reject publishing. Strict successor defaults are not silently enabled.
NIP-46 remains ephemeral. NIP-11 omits unverified expiration, protected-event and
management capabilities pending additional integration coverage.

Remaining validation before production: owner-only signed NIP-86 requests and
bans, protected events, expiration, deletion and replacement against PostgreSQL.
The legacy backend's tag-value-only SQL matching and nontransactional replacement
also need resolution before this migration is ready to merge. Search remains #7.
The race detector reports races inside the old go-nostr test client's connection
shutdown; normal tests and vet pass. Client migration is still required.
