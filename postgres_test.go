package main

import (
	"context"
	"fiatjaf.com/nostr/khatru"
	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/nbd-wtf/go-nostr"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Uses the original event schema and reopens it before querying, exercising an
// upgrade against pre-existing rows rather than only a newly initialized store.
func TestPostgresMigration(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database")
	}
	db := postgresql.PostgresBackend{DatabaseURL: url}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("TRUNCATE event"); err != nil {
		t.Fatal(err)
	}
	owner := newKeypair(t)
	event := signedEvent(t, owner, 1, nil, 1)
	if err := db.SaveEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	reopened := postgresql.PostgresBackend{DatabaseURL: url}
	if err := reopened.Init(); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	relay := khatru.NewRelay()
	wireStore(relay, &reopened)
	ProtectGiftWraps(relay)
	server := httptest.NewServer(relay)
	defer server.Close()
	tr := &testRelay{url: "ws" + strings.TrimPrefix(server.URL, "http")}
	ids, closed := collect(t, tr.connect(t), nostr.Filter{IDs: []string{event.ID}})
	if closed != "" || len(ids) != 1 || ids[0] != event.ID {
		t.Fatalf("existing row: %v %s", ids, closed)
	}
}
