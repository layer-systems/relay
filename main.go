package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/fiatjaf/eventstore/postgresql"
	"github.com/fiatjaf/khatru"
)

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func main() {
	// create the relay instance
	relay := khatru.NewRelay()

	// set up some basic properties (will be returned on the NIP-11 endpoint)
	relay.Info.Name = getEnv("RELAY_NAME", "layer.systems relay")
	relay.Info.PubKey = getEnv("RELAY_PUBKEY", "480ec1a7516406090dc042ddf67780ef30f26f3a864e83b417c053a5a611c838")
	relay.Info.Description = getEnv("RELAY_DESCRIPTION", "this is a public relay")
	relay.Info.Icon = getEnv("RELAY_ICON", "https://external-content.duckduckgo.com/iu/?u=https%3A%2F%2Fliquipedia.net%2Fcommons%2Fimages%2F3%2F35%2FSCProbe.jpg&f=1&nofb=1&ipt=0cbbfef25bce41da63d910e86c3c343e6c3b9d63194ca9755351bb7c2efa3359&ipo=images")

	db := postgresql.PostgresBackend{DatabaseURL: getEnv("DATABASE_URL", "postgresql://postgres:postgres@db:5432/khatru-relay?sslmode=disable")}
	if err := db.Init(); err != nil {
		panic(err)
	}

	relay.StoreEvent = append(relay.StoreEvent, db.SaveEvent)
	relay.QueryEvents = append(relay.QueryEvents, db.QueryEvents)
	relay.CountEvents = append(relay.CountEvents, db.CountEvents)
	relay.DeleteEvent = append(relay.DeleteEvent, db.DeleteEvent)
	relay.ReplaceEvent = append(relay.ReplaceEvent, db.ReplaceEvent)

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

	// // you can request auth by rejecting an event or a request with the prefix "auth-required: "
	// relay.RejectFilter = append(relay.RejectFilter,
	// 	// built-in policies
	// 	policies.NoComplexFilters,

	// 	// define your own policies
	// 	func(ctx context.Context, filter nostr.Filter) (reject bool, msg string) {
	// 		if pubkey := khatru.GetAuthed(ctx); pubkey != "" {
	// 			log.Printf("request from %s\n", pubkey)
	// 			return false, ""
	// 		}
	// 		return true, "auth-required: only authenticated users can read from this relay"
	// 		// (this will cause an AUTH message to be sent and then a CLOSED message such that clients can
	// 		//  authenticate and then request again)
	// 	},
	// )

	mux := relay.Router()
	// set up other http handlers
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		fmt.Fprintf(w, `<b>welcome</b> to my relay!`)
	})

	// start the server
	fmt.Println("running on :3334")
	http.ListenAndServe(":3334", relay)
}
