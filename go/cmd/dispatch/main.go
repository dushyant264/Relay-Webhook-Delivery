// Dispatch is Relay's delivery engine. It runs two consumer loops:
//
//   - fan-out: one event message -> N durable delivery rows + N delivery messages
//   - delivery: rate-limit + circuit-breaker checks, HMAC signing, HTTP POST,
//     attempt recording, retry-ladder routing, DLQ on exhaustion
//
// Exposes /healthz and /metrics on PORT (default 8082).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/dushyant/relay/internal/config"
	"github.com/dushyant/relay/internal/guard"
	"github.com/dushyant/relay/internal/mq"
	"github.com/dushyant/relay/internal/sign"
	"github.com/dushyant/relay/internal/store"
)

var (
	fanoutTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "relay_fanout_events_total",
		Help: "Event messages fanned out, by result.",
	}, []string{"result"})
	deliveriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "relay_deliveries_total",
		Help: "Delivery attempts processed, by outcome.",
	}, []string{"outcome"})
	deliveryDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "relay_delivery_duration_seconds",
		Help:    "Subscriber HTTP round-trip time.",
		Buckets: prometheus.DefBuckets,
	})
)

type dispatcher struct {
	log   *slog.Logger
	db    *store.Store
	mq    *mq.Client
	guard *guard.Guard
	http  *http.Client
	cfg   config.Config
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "dispatch")
	cfg := config.Load("8082")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	d := &dispatcher{
		log: log, db: db, mq: mqc,
		guard: guard.New(cfg.RedisAddr, log),
		http:  &http.Client{Timeout: cfg.HTTPTimeout},
		cfg:   cfg,
	}
	defer d.guard.Close()

	events, err := mqc.Consume(mq.EventFanoutQueue, cfg.Prefetch)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	deliveries, err := mqc.Consume(mq.DeliveriesQueue, cfg.Prefetch)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}

	go d.consumeLoop(ctx, "fanout", events, d.handleFanout)
	go d.consumeLoop(ctx, "delivery", deliveries, d.handleDelivery)

	go func() {
		err := <-mqc.NotifyClose()
		log.Error("amqp connection lost, exiting", "err", err)
		os.Exit(1)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /metrics", promhttp.Handler())
	go func() {
		log.Info("metrics listening", "port", cfg.Port)
		if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	cancel()
	log.Info("shut down")
}

// consumeLoop drives one consumer. Handler errors nack with requeue after a
// short pause (transient infra errors); success and permanent errors ack.
func (d *dispatcher) consumeLoop(ctx context.Context, name string, msgs <-chan amqp.Delivery, handle func(context.Context, []byte) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				d.log.Error("consumer channel closed", "consumer", name)
				os.Exit(1)
			}
			if err := handle(ctx, m.Body); err != nil {
				d.log.Error("handler failed, requeueing", "consumer", name, "err", err)
				time.Sleep(time.Second) // avoid hot-looping on a persistent infra error
				m.Nack(false, true)
				continue
			}
			m.Ack(false)
		}
	}
}

// --- fan-out ---

func (d *dispatcher) handleFanout(ctx context.Context, body []byte) error {
	var msg mq.EventMsg
	if err := json.Unmarshal(body, &msg); err != nil {
		d.log.Error("poison event message, dropping", "body", string(body), "err", err)
		fanoutTotal.WithLabelValues("poison").Inc()
		return nil // ack: retrying a parse error can never succeed
	}

	event, err := d.db.EventByID(ctx, msg.EventID)
	if err != nil {
		return fmt.Errorf("load event %s: %w", msg.EventID, err)
	}
	if event == nil {
		d.log.Warn("event message references unknown event, dropping", "event_id", msg.EventID)
		fanoutTotal.WithLabelValues("unknown_event").Inc()
		return nil
	}

	// Idempotent: ON CONFLICT DO NOTHING returns only newly created deliveries,
	// so a redelivered event message fans out zero new work.
	ids, err := d.db.CreateDeliveries(ctx, event.ID, event.ApplicationID, event.EventType)
	if err != nil {
		return fmt.Errorf("create deliveries: %w", err)
	}
	for _, id := range ids {
		dm, _ := json.Marshal(mq.DeliveryMsg{DeliveryID: id, Attempt: 1})
		if err := d.mq.Publish(ctx, mq.DeliveriesExchange, mq.DeliveryRoutingKey, dm); err != nil {
			// Publish failed mid-batch: nack the event message. Already-published
			// deliveries are deduplicated on redelivery by the unique index.
			return fmt.Errorf("publish delivery %s: %w", id, err)
		}
	}

	fanoutTotal.WithLabelValues("ok").Inc()
	d.log.Info("fanned out", "event_id", event.ID, "event_type", event.EventType, "deliveries", len(ids))
	return nil
}

// --- delivery ---

func (d *dispatcher) handleDelivery(ctx context.Context, body []byte) error {
	var msg mq.DeliveryMsg
	if err := json.Unmarshal(body, &msg); err != nil {
		d.log.Error("poison delivery message, dropping", "body", string(body), "err", err)
		deliveriesTotal.WithLabelValues("poison").Inc()
		return nil
	}

	job, err := d.db.DeliveryJob(ctx, msg.DeliveryID)
	if err != nil {
		return fmt.Errorf("load delivery %s: %w", msg.DeliveryID, err)
	}
	if job == nil {
		deliveriesTotal.WithLabelValues("unknown_delivery").Inc()
		return nil
	}
	if job.Status == "succeeded" || job.Status == "dead" {
		// Redelivered message for a finished delivery: nothing to do.
		deliveriesTotal.WithLabelValues("already_final").Inc()
		return nil
	}
	if job.Disabled {
		errMsg := "endpoint disabled"
		deliveriesTotal.WithLabelValues("endpoint_disabled").Inc()
		return d.db.RecordAttempt(ctx, store.AttemptResult{
			DeliveryID: job.DeliveryID, AttemptNo: msg.Attempt, Success: false, Error: &errMsg,
		}, "dead")
	}

	// Circuit breaker: skip the HTTP call entirely while the endpoint is
	// quarantined. Does NOT consume an attempt — park and retry later.
	if d.guard.BreakerOpen(ctx, job.EndpointID) {
		deliveriesTotal.WithLabelValues("breaker_open").Inc()
		return d.reschedule(ctx, msg, "1m")
	}

	// Rate limit: over-budget deliveries are parked briefly, not dropped, and
	// also don't consume an attempt.
	if !d.guard.AllowRate(ctx, job.EndpointID, job.RateLimit) {
		deliveriesTotal.WithLabelValues("rate_limited").Inc()
		return d.reschedule(ctx, msg, "10s")
	}

	outcome := d.attempt(ctx, job, msg.Attempt)

	if outcome.Success {
		d.guard.RecordSuccess(ctx, job.EndpointID)
		deliveriesTotal.WithLabelValues("succeeded").Inc()
		return d.db.RecordAttempt(ctx, outcome, "succeeded")
	}

	d.guard.RecordFailure(ctx, job.EndpointID)

	if msg.Attempt >= d.cfg.MaxAttempts {
		// Retry budget exhausted: mark dead and surface in the DLQ.
		lastErr := ""
		if outcome.Error != nil {
			lastErr = *outcome.Error
		}
		dead, _ := json.Marshal(mq.DeadMsg{
			DeliveryID: job.DeliveryID, EventID: job.EventID,
			EndpointID: job.EndpointID, Attempts: msg.Attempt, LastError: lastErr,
		})
		if err := d.mq.Publish(ctx, mq.DLXExchange, mq.DeadRoutingKey, dead); err != nil {
			return fmt.Errorf("publish to dlq: %w", err)
		}
		deliveriesTotal.WithLabelValues("dead").Inc()
		d.log.Warn("delivery dead-lettered", "delivery_id", job.DeliveryID, "attempts", msg.Attempt)
		return d.db.RecordAttempt(ctx, outcome, "dead")
	}

	// Park in the backoff tier for this attempt; TTL expiry re-queues it.
	tier := mq.TierForAttempt(msg.Attempt)
	next, _ := json.Marshal(mq.DeliveryMsg{DeliveryID: msg.DeliveryID, Attempt: msg.Attempt + 1})
	if err := d.mq.Publish(ctx, mq.RetryExchange, tier.Name, next); err != nil {
		return fmt.Errorf("publish retry: %w", err)
	}
	deliveriesTotal.WithLabelValues("retried").Inc()
	return d.db.RecordAttempt(ctx, outcome, "failed")
}

// attempt performs one signed HTTP POST and returns the audit record.
func (d *dispatcher) attempt(ctx context.Context, job *store.DeliveryJob, attemptNo int) store.AttemptResult {
	res := store.AttemptResult{DeliveryID: job.DeliveryID, AttemptNo: attemptNo}

	envelope, _ := json.Marshal(map[string]any{
		"id":        job.EventID,
		"type":      job.EventType,
		"timestamp": job.ReceivedAt.UTC().Format(time.RFC3339),
		"attempt":   attemptNo,
		"data":      json.RawMessage(job.Payload),
	})

	now := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.URL, bytes.NewReader(envelope))
	if err != nil {
		msg := err.Error()
		res.Error = &msg
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Relay-Webhooks/1.0")
	req.Header.Set("Relay-Id", job.DeliveryID)
	req.Header.Set("Relay-Event", job.EventType)
	req.Header.Set("Relay-Timestamp", strconv.FormatInt(now.Unix(), 10))
	req.Header.Set("Relay-Signature", sign.Sign(job.Secret, job.DeliveryID, now, envelope))

	start := time.Now()
	resp, err := d.http.Do(req)
	res.DurationMS = int(time.Since(start).Milliseconds())
	deliveryDuration.Observe(time.Since(start).Seconds())

	if err != nil {
		msg := err.Error()
		res.Error = &msg
		return res
	}
	defer resp.Body.Close()

	res.StatusCode = &resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.Success = true
	} else {
		msg := "non-2xx response: " + resp.Status
		res.Error = &msg
	}
	return res
}

// reschedule parks a message in a named retry tier WITHOUT consuming an
// attempt (used for rate limiting and open breakers).
func (d *dispatcher) reschedule(ctx context.Context, msg mq.DeliveryMsg, tier string) error {
	body, _ := json.Marshal(msg)
	return d.mq.Publish(ctx, mq.RetryExchange, tier, body)
}
