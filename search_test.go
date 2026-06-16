package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/nbd-wtf/go-nostr"
)

func TestNormalizeSearchQueryIgnoresExtensions(t *testing.T) {
	phrase, terms := normalizeSearchQuery("include:spam best nostr apps domain:example.com")

	if phrase != "best nostr apps" {
		t.Fatalf("phrase = %q, want %q", phrase, "best nostr apps")
	}
	if !reflect.DeepEqual(terms, []string{"best", "nostr", "apps"}) {
		t.Fatalf("terms = %#v", terms)
	}
}

func TestSearchEventsSQLOrdersByScoreBeforeLimit(t *testing.T) {
	db := &postgresql.PostgresBackend{
		QueryLimit:        100,
		QueryIDsLimit:     500,
		QueryAuthorsLimit: 500,
		QueryKindsLimit:   10,
		QueryTagsLimit:    10,
	}

	query, params, err := searchEventsSQL(db, nostr.Filter{
		Kinds:  []int{nostr.KindTextNote},
		Search: "include:spam Purple nostr",
		Limit:  5,
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(query, "ORDER BY (CASE WHEN content ILIKE") {
		t.Fatalf("query does not order by score: %s", query)
	}
	if !strings.Contains(query, "DESC, created_at DESC, id LIMIT") {
		t.Fatalf("query does not apply limit after score ordering: %s", query)
	}

	expectedParams := []any{
		nostr.KindTextNote,
		"%Purple nostr%",
		"%Purple%",
		"%nostr%",
		"%Purple nostr%",
		"%Purple%",
		"%nostr%",
		5,
	}
	if !reflect.DeepEqual(params, expectedParams) {
		t.Fatalf("params = %#v, want %#v", params, expectedParams)
	}
}
