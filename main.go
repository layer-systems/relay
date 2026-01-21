package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/fiatjaf/khatru"
	"github.com/fiatjaf/khatru/policies"
	_ "github.com/lib/pq"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip86"
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
	relay.Info.PubKey = getEnv("RELAY_PUBKEY", "480ec1a7516406090dc042ddf67780ef30f26f3a864e83b417c053a5a611c838")
	relay.Info.Description = getEnv("RELAY_DESCRIPTION", "this is a public relay")
	relay.Info.Icon = getEnv("RELAY_ICON", "https://external-content.duckduckgo.com/iu/?u=https%3A%2F%2Fliquipedia.net%2Fcommons%2Fimages%2F3%2F35%2FSCProbe.jpg&f=1&nofb=1&ipt=0cbbfef25bce41da63d910e86c3c343e6c3b9d63194ca9755351bb7c2efa3359&ipo=images")

	relay.Info.Software = "https://github.com/layer-systems/relay"
	relay.Info.Version = "0.1.0"
	relay.Info.SupportedNIPs = []any{1, 11, 17, 40, 42, 70, 86}

	// Open shared database connection with aggressive pool limits
	managementDB, err := sql.Open("postgres", getEnv("DATABASE_URL", "postgresql://postgres:postgres@db:5432/khatru-relay?sslmode=disable"))
	if err != nil {
		panic(err)
	}
	defer managementDB.Close()

	// Configure connection pool to prevent "too many clients" errors
	managementDB.SetMaxOpenConns(10)                   // Maximum number of open connections
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

	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.CountEvents = append(relay.CountEvents, db.CountEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)
	relay.ReplaceEvent = append(relay.ReplaceEvent, db.ReplaceEvent)

	relay.RejectEvent = append(relay.RejectEvent, policies.ValidateKind)

	relay.RejectEvent = append(relay.RejectEvent,
		func(ctx context.Context, event *nostr.Event) (reject bool, msg string) {
			var reason string
			row := managementDB.QueryRowContext(ctx, `SELECT reason FROM banned_pubkeys WHERE pubkey = $1`, event.PubKey)
			switch err := row.Scan(&reason); err {
			case sql.ErrNoRows:
				return false, ""
			case nil:
				return true, fmt.Sprintf("pubkey %s banned: %s", event.PubKey, reason)
			default:
				// on unexpected DB errors, do not reject the event solely because of the failure
				return false, ""
			}
		},
	)

	// // there are many other configurable things you can set
	// relay.RejectEvent = append(relay.RejectEvent,
	// 	// built-in policies
	// 	policies.ValidateKind,

	// 	// define your own policies
	// 	policies.PreventLargeTags(100),
	// 	func(ctx context.Context, event *nostr.Event) (reject bool, msg string) {
	// 		if event.PubKey == "fa984bd7dbb282f07e16e7ae87b26a2a7b9b90b7246a44771f0cf5ae58018f52" {
	// 			return true, "we don't allow this person to write here"
	// 		}
	// 		return false, "" // anyone else can
	// 	},
	// )

	relay.RejectFilter = append(relay.RejectFilter, RejectNonAuthenticatedGiftWrapQueries)

	// management endpoints
	relay.ManagementAPI.RejectAPICall = append(relay.ManagementAPI.RejectAPICall,
		func(ctx context.Context, mp nip86.MethodParams) (reject bool, msg string) {
			user := khatru.GetAuthed(ctx)
			ownerPubKey := getEnv("RELAY_PUBKEY", "480ec1a7516406090dc042ddf67780ef30f26f3a864e83b417c053a5a611c838")
			if user != ownerPubKey {
				return true, "auth-required: only relay owner can access management API"
			}
			return false, ""
		})

	relay.ManagementAPI.AllowPubKey = func(ctx context.Context, pubkey string, reason string) error {
		_, err := managementDB.Exec(`
			INSERT INTO allowed_pubkeys (pubkey, reason, created_at) 
			VALUES ($1, $2, $3)
			ON CONFLICT (pubkey) DO UPDATE SET reason = $2, created_at = $3
		`, pubkey, reason, time.Now())
		return err
	}

	relay.ManagementAPI.BanPubKey = func(ctx context.Context, pubkey string, reason string) error {
		_, err := managementDB.Exec(`
			INSERT INTO banned_pubkeys (pubkey, reason, created_at) 
			VALUES ($1, $2, $3)
			ON CONFLICT (pubkey) DO UPDATE SET reason = $2, created_at = $3
		`, pubkey, reason, time.Now())
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
			if err := rows.Scan(&pk.PubKey, &pk.Reason); err != nil {
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
			if err := rows.Scan(&pk.PubKey, &pk.Reason); err != nil {
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
