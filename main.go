package main

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"net/http"
	"os"
	"strconv"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/khatru/policies"
	"fiatjaf.com/nostr/nip86"
	"github.com/fiatjaf/eventstore/postgresql"
	_ "github.com/lib/pq"
)

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func initManagementDB(db *sql.DB) error {
	// create allowed_pubkeys table
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS allowed_pubkeys (
			pubkey TEXT PRIMARY KEY,
			reason TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create allowed_pubkeys table: %w", err)
	}

	// create banned_pubkeys table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS banned_pubkeys (
			pubkey TEXT PRIMARY KEY,
			reason TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create banned_pubkeys table: %w", err)
	}

	return nil
}

func main() {
	// create the relay instance
	relay := khatru.NewRelay()

	// set up some basic properties (will be returned on the NIP-11 endpoint)
	relay.Info.Name = getEnv("RELAY_NAME", "layer.systems relay")
	owner, err := nostr.PubKeyFromHex(getEnv("RELAY_PUBKEY", "480ec1a7516406090dc042ddf67780ef30f26f3a864e83b417c053a5a611c838"))
	if err != nil {
		panic(err)
	}
	relay.Info.PubKey = &owner
	relay.Info.Description = getEnv("RELAY_DESCRIPTION", "this is a public relay")
	relay.Info.Icon = getEnv("RELAY_ICON", "https://external-content.duckduckgo.com/iu/?u=https%3A%2F%2Fliquipedia.net%2Fcommons%2Fimages%2F3%2F35%2FSCProbe.jpg&f=1&nofb=1&ipt=0cbbfef25bce41da63d910e86c3c343e6c3b9d63194ca9755351bb7c2efa3359&ipo=images")

	relay.Info.Software = "https://github.com/layer-systems/relay"
	relay.Info.Version = "0.1.1"
	relay.Info.SupportedNIPs = []any{1, 11, 17, 42, 46}

	// Open shared database connection with aggressive pool limits
	managementDB, err := sql.Open("postgres", getEnv("DATABASE_URL", "postgresql://postgres:postgres@db:5432/khatru-relay?sslmode=disable"))
	if err != nil {
		panic(err)
	}
	defer managementDB.Close()

	// Configure connection pool to prevent "too many clients" errors
	managementDB.SetMaxOpenConns(10)                  // Maximum number of open connections
	managementDB.SetMaxIdleConns(5)                   // Maximum number of idle connections
	managementDB.SetConnMaxLifetime(3 * time.Minute)  // Maximum lifetime of a connection
	managementDB.SetConnMaxIdleTime(30 * time.Second) // Maximum idle time of a connection

	// Initialize management tables
	if err := initManagementDB(managementDB); err != nil {
		panic(err)
	}

	// Setup event store backend with shared DB connection
	queryLimit, _ := strconv.Atoi(getEnv("QUERY_LIMIT", "100"))
	db := postgresql.PostgresBackend{DatabaseURL: getEnv("DATABASE_URL", "postgresql://postgres:postgres@db:5432/khatru-relay?sslmode=disable"), QueryLimit: queryLimit}
	if err := db.Init(); err != nil {
		panic(err)
	}

	wireStore(relay, &db)
	defer db.Close()
	relay.StartExpirationManager(func(ctx context.Context, f nostr.Filter) iter.Seq[nostr.Event] { return relay.QueryStored(ctx, f) }, relay.DeleteEvent, nil)
	relay.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		if event.Kind == 20001 {
			return true, "kind 20001 events are not allowed"
		}
		if reject, msg := policies.ValidateKind(ctx, event); reject {
			return true, msg
		}
		var reason string
		err := managementDB.QueryRowContext(ctx, `SELECT reason FROM banned_pubkeys WHERE pubkey = $1`, event.PubKey.Hex()).Scan(&reason)
		if err == nil {
			return true, "restricted: pubkey banned: " + reason
		}
		if err != sql.ErrNoRows {
			return true, "error: ban lookup failed"
		}
		return false, ""
	}

	// NIP-17: only authenticated recipients may read kind 1059 gift wraps
	// (stored REQ, live subscriptions, COUNT and negentropy).
	// Must run after every QueryEvents/CountEvents source has been registered.
	ProtectGiftWraps(relay)

	// management endpoints
	relay.ManagementAPI.OnAPICall =
		func(ctx context.Context, mp nip86.MethodParams) (reject bool, msg string) {
			user, authenticated := khatru.GetAuthed(ctx)
			ownerPubKey := getEnv("RELAY_PUBKEY", "480ec1a7516406090dc042ddf67780ef30f26f3a864e83b417c053a5a611c838")
			if !authenticated || user.Hex() != ownerPubKey {
				return true, "auth-required: only relay owner can access management API"
			}
			return false, ""
		}

	relay.ManagementAPI.AllowPubKey = func(ctx context.Context, pubkey nostr.PubKey, reason string) error {
		_, err := managementDB.Exec(`
			INSERT INTO allowed_pubkeys (pubkey, reason, created_at) 
			VALUES ($1, $2, $3)
			ON CONFLICT (pubkey) DO UPDATE SET reason = $2, created_at = $3
		`, pubkey.Hex(), reason, time.Now())
		return err
	}

	relay.ManagementAPI.BanPubKey = func(ctx context.Context, pubkey nostr.PubKey, reason string) error {
		_, err := managementDB.Exec(`
			INSERT INTO banned_pubkeys (pubkey, reason, created_at) 
			VALUES ($1, $2, $3)
			ON CONFLICT (pubkey) DO UPDATE SET reason = $2, created_at = $3
		`, pubkey.Hex(), reason, time.Now())
		return err
	}

	relay.ManagementAPI.ListAllowedPubKeys = func(ctx context.Context) ([]nip86.PubKeyReason, error) {
		rows, err := managementDB.Query(`SELECT pubkey, reason FROM allowed_pubkeys ORDER BY created_at DESC`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []nip86.PubKeyReason
		for rows.Next() {
			var pk nip86.PubKeyReason
			var hex string
			if err := rows.Scan(&hex, &pk.Reason); err != nil {
				return nil, err
			}
			pk.PubKey, err = nostr.PubKeyFromHex(hex)
			if err != nil {
				return nil, err
			}
			result = append(result, pk)
		}
		return result, rows.Err()
	}

	relay.ManagementAPI.ListBannedPubKeys = func(ctx context.Context) ([]nip86.PubKeyReason, error) {
		rows, err := managementDB.Query(`SELECT pubkey, reason FROM banned_pubkeys ORDER BY created_at DESC`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []nip86.PubKeyReason
		for rows.Next() {
			var pk nip86.PubKeyReason
			var hex string
			if err := rows.Scan(&hex, &pk.Reason); err != nil {
				return nil, err
			}
			pk.PubKey, err = nostr.PubKeyFromHex(hex)
			if err != nil {
				return nil, err
			}
			result = append(result, pk)
		}
		return result, rows.Err()
	}

	mux := relay.Router()
	// set up other http handlers
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		fmt.Fprintf(w, `Please use a <b>nostr</b> client!`)
	})

	// start the server
	fmt.Println("running on :3334")
	http.ListenAndServe(":3334", relay)
}
