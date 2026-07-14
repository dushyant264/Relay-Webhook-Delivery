// Receiver is a demo webhook subscriber used to exercise Relay end-to-end.
// It verifies Relay signatures and can simulate flaky endpoints:
//
//	POST /webhook            verify + 200
//	POST /webhook?fail=3     fail the first 3 attempts of each delivery, then succeed
//	POST /webhook?fail=always  always 500 (drives the DLQ / circuit-breaker path)
package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dushyant/relay/internal/sign"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "receiver")
	secret := os.Getenv("RECEIVER_SECRET")
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}

	var mu sync.Mutex
	attempts := map[string]int{} // Relay-Id -> attempts seen

	http.HandleFunc("POST /webhook", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		id := r.Header.Get("Relay-Id")

		tsUnix, _ := strconv.ParseInt(r.Header.Get("Relay-Timestamp"), 10, 64)
		if secret != "" {
			err := sign.Verify(secret, id, r.Header.Get("Relay-Signature"),
				time.Unix(tsUnix, 0), body, 5*time.Minute)
			if err != nil {
				log.Error("SIGNATURE REJECTED", "relay_id", id, "err", err)
				http.Error(w, "bad signature", http.StatusUnauthorized)
				return
			}
		}

		mu.Lock()
		attempts[id]++
		n := attempts[id]
		mu.Unlock()

		failParam := r.URL.Query().Get("fail")
		failN, _ := strconv.Atoi(failParam)
		if failParam == "always" || n <= failN {
			log.Warn("simulating failure", "relay_id", id, "attempt", n, "event", r.Header.Get("Relay-Event"))
			http.Error(w, "simulated failure", http.StatusInternalServerError)
			return
		}

		log.Info("webhook received ✓ signature verified", "relay_id", id, "attempt", n,
			"event", r.Header.Get("Relay-Event"), "body", string(body))
		w.WriteHeader(http.StatusOK)
	})

	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Info("receiver listening", "port", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
