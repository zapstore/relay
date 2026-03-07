package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/gorilla/websocket"
	_ "github.com/joho/godotenv/autoload"
	"github.com/nbd-wtf/go-nostr"
	"github.com/zapstore/relay/pkg/events"
)

// Config holds the stress test configuration, loaded from a .env file.
type Config struct {
	Port         string        `env:"STRESS_PORT"`
	NumClients   int           `env:"STRESS_CLIENTS"`
	Duration     time.Duration `env:"STRESS_DURATION"`
	ReqRatio     float64       `env:"STRESS_REQ_RATIO"`
	MsgPerSecond float64       `env:"STRESS_MSG_PER_SECOND"`
}

var config Config

func init() {
	config = Config{
		Port:         "3334",
		NumClients:   1000,
		Duration:     30 * time.Second,
		ReqRatio:     0.99,
		MsgPerSecond: 1,
	}

	if err := env.Parse(&config); err != nil {
		panic(err)
	}
}

// The valid Zapstore event app
var ZapstoreEvent = nostr.Event{
	ID:        "21aa3ed33c82d2636622dfd78d106d783843dc97f24b5b66d98c3c363ebbf97b",
	PubKey:    "78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d",
	CreatedAt: nostr.Timestamp(1770780418),
	Kind:      32267,
	Tags: nostr.Tags{
		{"name", "Zapstore"},
		{"d", "dev.zapstore.app"},
		{"repository", "https://github.com/zapstore/zapstore"},
		{"url", "https://zapstore.dev"},
		{"f", "android-arm64-v8a"},
		{"t", "android"},
		{"t", "apk"},
		{"t", "app"},
		{"t", "appstore"},
		{"t", "grapheneos"},
		{"t", "lightning"},
		{"t", "lightning-network"},
		{"t", "nostr"},
		{"t", "obtainium"},
		{"t", "permissionless"},
		{"t", "playstore"},
		{"t", "sha256"},
		{"t", "social-graph"},
		{"t", "weboftrust"},
		{"license", "MIT"},
		{"icon", "https://cdn.zapstore.dev/2787fabd17260808c72ec5456996dbd5356bc8e822a1ecf85f220a29dbe2e998"},
		{"a", "30063:78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d:dev.zapstore.app@1.0.0"},
	},
	Content: "The open app store powered by your social network",
	Sig:     "e2e3e2b4b3bd120cc55c34023f037685a3128bb35c2410b357a684c368c4fdb6986c98df9675e3cb9bf48608af1753c80a92df5a6214a6a1497721c52455db76",
}

// Zapstore typical filters
var ZapstoreFilters = []nostr.Filters{
	{{
		// Single filter, looking for specific apps with d-tags
		Kinds:   []int{events.KindApp},
		Authors: []string{"78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"},
		Tags:    nostr.TagMap{"d": []string{"com.signal.app", "net.bible.android.activity", "net.mullvad.mullvadvpn", "spam.blocker"}},
		Limit:   4,
	}},
	{{
		// Single filter, looking for specific apps with search
		Kinds:  []int{events.KindApp},
		Tags:   nostr.TagMap{"f": []string{"android-arm64-v8a"}},
		Search: "white",
		Limit:  16,
	}},
	{
		// Multiple filters, each looking for one set
		{
			Kinds:   []int{events.KindAppSet},
			Authors: []string{"78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"},
			Tags:    nostr.TagMap{"i": []string{"com.quietmobile"}},
			Limit:   1,
		},
		{
			Kinds:   []int{events.KindAppSet},
			Authors: []string{"78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"},
			Tags:    nostr.TagMap{"i": []string{"fr.mobdev.peertubelive"}},
			Limit:   1,
		},
		{
			Kinds:   []int{events.KindAppSet},
			Authors: []string{"78ce6faa72264387284e647ba6938995735ec8c7d5c5a65737e55130f026307d"},
			Tags:    nostr.TagMap{"i": []string{"com.rekna.knocky"}},
			Limit:   1,
		},
	},
}

func TestRelayStress(t *testing.T) {
	t.Logf("stress config: port=%s clients=%d duration=%s req_ratio=%.2f msg_per_second=%.2f",
		config.Port, config.NumClients, config.Duration, config.ReqRatio, config.MsgPerSecond)

	interval := time.Duration(float64(time.Second) / config.MsgPerSecond)

	ctx, cancel := context.WithTimeout(context.Background(), config.Duration)
	defer cancel()

	wg := sync.WaitGroup{}
	connFails := atomic.Int64{}
	sendFails := atomic.Int64{}
	sent := atomic.Int64{}
	received := atomic.Int64{}
	start := time.Now()

	for i := range config.NumClients {
		wg.Add(1)
		go func(clientID int) {
			defer wg.Done()

			conn, _, err := websocket.DefaultDialer.DialContext(
				ctx, fmt.Sprintf("ws://localhost:%s", config.Port), nil,
			)
			if err != nil {
				connFails.Add(1)
				log.Printf("client %d: connect error: %v", clientID, err)
				return
			}
			defer conn.Close()

			// Read all incoming messages so the relay is never blocked writing to us.
			go func() {
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					received.Add(1)
				}
			}()

			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for j := 0; ; j++ {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// proceed and write message
				}

				var (
					msg []byte
					err error
				)

				if rand.Float64() < config.ReqRatio {
					// REQ: pick a random filter set from ZapstoreFilters and spread its filters into the message.
					subID := fmt.Sprintf("s-%d-%d", clientID, j)
					filters := ZapstoreFilters[rand.IntN(len(ZapstoreFilters))]

					req := []any{"REQ", subID}
					for _, f := range filters {
						req = append(req, f)
					}
					msg, err = json.Marshal(req)
				} else {
					// EVENT: publish the Zapstore event.
					msg, err = json.Marshal([]any{"EVENT", ZapstoreEvent})
				}

				if err != nil {
					sendFails.Add(1)
					continue
				}

				if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
					sendFails.Add(1)
					return
				}
				sent.Add(1)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(start)

	t.Logf("finished in %s", elapsed)
	t.Logf("sent=%d received=%d conn_failures=%d send_failures=%d throughput=%.0f msg/s",
		sent.Load(), received.Load(), connFails.Load(), sendFails.Load(), float64(sent.Load())/elapsed.Seconds())
}
