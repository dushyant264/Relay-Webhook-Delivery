// Ingest is Relay's event intake service — the hot path.
//
//	POST /v1/events   accept an event (API key auth, idempotent), publish to RabbitMQ
//	GET  /healthz     liveness
//	GET  /metrics     prometheus
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dushyant/relay/internal/config"
	"github.com/dushyant/relay/internal/guard"
	"github.com/dushyant/relay/internal/mq"
	"github.com/dushyant/relay/internal/store"
)

const keyPrefixLen = 16

var eventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "relay_ingest_events_total",
	Help: "Events received by result.",
}, []string{"result"})

type server struct {
	log   *slog.Logger
	db    *store.Store
	mq    *mq.Client
	guard *guard.Guard
}

type eventRequest struct {
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "ingest")
	cfg := config.Load("8081")
	ctx := context.Background()

	db, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	mqc, err := mq.Connect(cfg.AMQPURL)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	defer mqc.Close()

	g := guard.New(cfg.RedisAddr, log)
	defer g.Close()

	s := &server{log: log, db: db, mq: mqc, guard: g}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", s.handleEvent)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /metrics", promhttp.Handler())

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		// Crash-only recovery: if the broker connection drops, exit and let the
		// orchestrator restart us with a clean connection.
		err := <-mqc.NotifyClose()
		log.Error("amqp connection lost, exiting", "err", err)
		os.Exit(1)
	}()

	go func() {
		log.Info("listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	log.Info("shut down")
}

// authenticate resolves the bearer API key to an application id, using Redis
// as a read-through cache in front of Postgres. Hash comparison is constant-time.
func (s *server) authenticate(r *http.Request) (string, error) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(raw) <= keyPrefixLen || !strings.HasPrefix(raw, "relay_sk_") {
		return "", errors.New("missing or malformed API key")
	}
	prefix := raw[:keyPrefixLen]

	var appID, keyHash string
	if cached, ok := s.guard.CacheGet(r.Context(), prefix); ok {
		appID, keyHash, _ = strings.Cut(cached, "|")
	} else {
		key, err := s.db.APIKeyByPrefix(r.Context(), prefix)
		if err != nil {
			return "", fmt.Errorf("key lookup: %w", err)
		}
		if key == nil {
			return "", errors.New("unknown or revoked API key")
		}
		appID, keyHash = key.ApplicationID, key.KeyHash
		s.guard.CacheSet(r.Context(), prefix, appID+"|"+keyHash, time.Minute)
	}

	sum := sha256.Sum256([]byte(raw))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(keyHash)) != 1 {
		return "", errors.New("invalid API key")
	}
	return appID, nil
}

func (s *server) handleEvent(w http.ResponseWriter, r *http.Request) {
	appID, err := s.authenticate(r)
	if err != nil {
		eventsTotal.WithLabelValues("unauthorized").Inc()
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req eventRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		eventsTotal.WithLabelValues("bad_request").Inc()
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.EventType == "" || len(req.Payload) == 0 {
		eventsTotal.WithLabelValues("bad_request").Inc()
		writeErr(w, http.StatusBadRequest, "event_type and payload are required")
		return
	}

	known, err := s.db.EventTypeExists(r.Context(), appID, req.EventType)
	if err != nil {
		s.internalErr(w, "event type check", err)
		return
	}
	if !known {
		eventsTotal.WithLabelValues("unknown_type").Inc()
		writeErr(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("event type %q is not registered for this application", req.EventType))
		return
	}

	var idemKey *string
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		idemKey = &k
	}

	eventID, duplicate, err := s.db.InsertEvent(r.Context(), appID, req.EventType, req.Payload, idemKey)
	if err != nil {
		s.internalErr(w, "insert event", err)
		return
	}
	if duplicate {
		// Producer retry of a request we already accepted: return the original
		// id without publishing again.
		eventsTotal.WithLabelValues("duplicate").Inc()
		writeJSON(w, http.StatusOK, map[string]any{"event_id": eventID, "duplicate": true})
		return
	}

	body, _ := json.Marshal(mq.EventMsg{EventID: eventID})
	if err := s.mq.Publish(r.Context(), mq.EventsExchange, mq.EventRoutingKey, body); err != nil {
		// Event is persisted but not queued. Surface a 5xx so the producer
		// retries with the same Idempotency-Key; see overview doc trade-offs.
		s.internalErr(w, "publish event", err)
		return
	}

	eventsTotal.WithLabelValues("accepted").Inc()
	writeJSON(w, http.StatusAccepted, map[string]any{"event_id": eventID})
}

func (s *server) internalErr(w http.ResponseWriter, what string, err error) {
	eventsTotal.WithLabelValues("error").Inc()
	s.log.Error(what, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
