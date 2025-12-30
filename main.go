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

	// create reports table for NIP-56
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS reports (
			id TEXT PRIMARY KEY,
			reporter_pubkey TEXT NOT NULL,
			reported_event_id TEXT,
			reported_pubkey TEXT NOT NULL,
			report_type TEXT NOT NULL,
			content TEXT,
			resolved BOOLEAN NOT NULL DEFAULT FALSE,
			resolution TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create reports table: %w", err)
	}

	// create banned_events table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS banned_events (
			event_id TEXT PRIMARY KEY,
			reason TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create banned_events table: %w", err)
	}

	// create allowed_events table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS allowed_events (
			event_id TEXT PRIMARY KEY,
			reason TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create allowed_events table: %w", err)
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

	// setup management database (second connection for NIP-86)
	managementDB, err := sql.Open("postgres", getEnv("DATABASE_URL", "postgresql://postgres:postgres@db:5432/khatru-relay?sslmode=disable"))
	if err != nil {
		panic(err)
	}
	defer managementDB.Close()

	// initialize management tables
	if err := initManagementDB(managementDB); err != nil {
		panic(err)
	}

	// Store NIP-56 report events
	relay.StoreEvent = append(relay.StoreEvent, func(ctx context.Context, event *nostr.Event) error {
		if event.Kind == 1984 {
			// Parse report tags
			var reportedEventId, reportedPubkey, reportType string

			// Extract reported pubkey and type from p tag
			if pTag := event.Tags.Find("p"); len(pTag) >= 2 {
				reportedPubkey = pTag[1]
				if len(pTag) >= 3 {
					reportType = pTag[2]
				}
			}

			// Extract reported event id and type from e tag
			if eTag := event.Tags.Find("e"); len(eTag) >= 2 {
				reportedEventId = eTag[1]
				if len(eTag) >= 3 && reportType == "" {
					reportType = eTag[2]
				}
			}

			if reportedPubkey != "" && reportType != "" {
				_, err := managementDB.ExecContext(ctx, `
					INSERT INTO reports (id, reporter_pubkey, reported_event_id, reported_pubkey, report_type, content, created_at)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
					ON CONFLICT (id) DO NOTHING
				`, event.ID, event.PubKey, reportedEventId, reportedPubkey, reportType, event.Content, time.Unix(int64(event.CreatedAt), 0))
				if err != nil {
					return fmt.Errorf("failed to store report: %w", err)
				}
			}
		}
		return nil
	})

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

	// Check for banned events
	relay.RejectEvent = append(relay.RejectEvent,
		func(ctx context.Context, event *nostr.Event) (reject bool, msg string) {
			var reason string
			row := managementDB.QueryRowContext(ctx, `SELECT reason FROM banned_events WHERE event_id = $1`, event.ID)
			switch err := row.Scan(&reason); err {
			case sql.ErrNoRows:
				return false, ""
			case nil:
				return true, fmt.Sprintf("event %s banned: %s", event.ID, reason)
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

	relay.ManagementAPI.ListEventsNeedingModeration = func(ctx context.Context) ([]nip86.IDReason, error) {
		rows, err := managementDB.Query(`
			SELECT COALESCE(reported_event_id, reported_pubkey), 
			       CONCAT(report_type, ': ', content, ' (reported by ', reporter_pubkey, ')')
			FROM reports 
			WHERE resolved = FALSE 
			ORDER BY created_at DESC
		`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []nip86.IDReason
		for rows.Next() {
			var ir nip86.IDReason
			if err := rows.Scan(&ir.ID, &ir.Reason); err != nil {
				return nil, err
			}
			result = append(result, ir)
		}
		return result, rows.Err()
	}

	relay.ManagementAPI.AllowEvent = func(ctx context.Context, id string, reason string) error {
		// Mark reports about this event as resolved
		_, err := managementDB.Exec(`
			UPDATE reports 
			SET resolved = TRUE, resolution = $2 
			WHERE reported_event_id = $1 OR reported_pubkey = $1
		`, id, "allowed: "+reason)
		if err != nil {
			return err
		}

		// Add to allowed_events table
		_, err = managementDB.Exec(`
			INSERT INTO allowed_events (event_id, reason, created_at) 
			VALUES ($1, $2, $3)
			ON CONFLICT (event_id) DO UPDATE SET reason = $2, created_at = $3
		`, id, reason, time.Now())
		return err
	}

	relay.ManagementAPI.BanEvent = func(ctx context.Context, id string, reason string) error {
		// Mark reports about this event as resolved
		_, err := managementDB.Exec(`
			UPDATE reports 
			SET resolved = TRUE, resolution = $2 
			WHERE reported_event_id = $1 OR reported_pubkey = $1
		`, id, "banned: "+reason)
		if err != nil {
			return err
		}

		// Add to banned_events table
		_, err = managementDB.Exec(`
			INSERT INTO banned_events (event_id, reason, created_at) 
			VALUES ($1, $2, $3)
			ON CONFLICT (event_id) DO UPDATE SET reason = $2, created_at = $3
		`, id, reason, time.Now())
		if err != nil {
			return err
		}

		// Query and delete the event from the main event store (if it exists)
		for _, query := range relay.QueryEvents {
			ch, err := query(ctx, nostr.Filter{IDs: []string{id}})
			if err != nil {
				continue
			}

			// Read all events from the channel and delete them
			for evt := range ch {
				if evt != nil {
					for _, deleter := range relay.DeleteEvent {
						if err := deleter(ctx, evt); err != nil {
							return fmt.Errorf("failed to delete event: %w", err)
						}
					}
				}
			}
			// Event successfully deleted or didn't exist - either way, ban is in place
			break
		}

		return nil
	}

	relay.ManagementAPI.ListBannedEvents = func(ctx context.Context) ([]nip86.IDReason, error) {
		rows, err := managementDB.Query(`SELECT event_id, reason FROM banned_events ORDER BY created_at DESC`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []nip86.IDReason
		for rows.Next() {
			var ir nip86.IDReason
			if err := rows.Scan(&ir.ID, &ir.Reason); err != nil {
				return nil, err
			}
			result = append(result, ir)
		}
		return result, rows.Err()
	}

	relay.ManagementAPI.ListAllowedEvents = func(ctx context.Context) ([]nip86.IDReason, error) {
		rows, err := managementDB.Query(`SELECT event_id, reason FROM allowed_events ORDER BY created_at DESC`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var result []nip86.IDReason
		for rows.Next() {
			var ir nip86.IDReason
			if err := rows.Scan(&ir.ID, &ir.Reason); err != nil {
				return nil, err
			}
			result = append(result, ir)
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
