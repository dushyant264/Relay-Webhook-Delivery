// Package store is the Postgres data access layer shared by ingest and dispatch.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool *pgxpool.Pool
}

func Connect(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// --- ingest ---

type APIKey struct {
	ApplicationID string
	KeyHash       string
}

func (s *Store) APIKeyByPrefix(ctx context.Context, prefix string) (*APIKey, error) {
	var k APIKey
	err := s.Pool.QueryRow(ctx,
		`select application_id, key_hash from api_keys
		 where prefix = $1 and revoked_at is null`, prefix).
		Scan(&k.ApplicationID, &k.KeyHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (s *Store) EventTypeExists(ctx context.Context, appID, name string) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx,
		`select exists(select 1 from event_types where application_id = $1 and name = $2)`,
		appID, name).Scan(&exists)
	return exists, err
}

// InsertEvent persists an event idempotently. If the (application, idempotency
// key) pair was seen before, the original event's id is returned with
// duplicate=true and no new row is created.
func (s *Store) InsertEvent(ctx context.Context, appID, eventType string, payload json.RawMessage, idemKey *string) (id string, duplicate bool, err error) {
	err = s.Pool.QueryRow(ctx,
		`insert into events (application_id, event_type, payload, idempotency_key)
		 values ($1, $2, $3, $4)
		 on conflict (application_id, idempotency_key) where idempotency_key is not null
		 do nothing
		 returning id`,
		appID, eventType, payload, idemKey).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	// Conflict: fetch the existing event id for this idempotency key.
	err = s.Pool.QueryRow(ctx,
		`select id from events where application_id = $1 and idempotency_key = $2`,
		appID, *idemKey).Scan(&id)
	return id, true, err
}

// --- dispatch: fan-out ---

type Event struct {
	ID            string
	ApplicationID string
	EventType     string
	Payload       json.RawMessage
	ReceivedAt    time.Time
}

func (s *Store) EventByID(ctx context.Context, id string) (*Event, error) {
	var e Event
	err := s.Pool.QueryRow(ctx,
		`select id, application_id, event_type, payload, received_at from events where id = $1`, id).
		Scan(&e.ID, &e.ApplicationID, &e.EventType, &e.Payload, &e.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// CreateDeliveries fans an event out to every subscribed, enabled endpoint.
// ON CONFLICT DO NOTHING makes redelivered event messages a no-op: only ids of
// NEWLY created deliveries are returned, so they are published exactly once
// per fan-out (at-least-once overall).
func (s *Store) CreateDeliveries(ctx context.Context, eventID, appID, eventType string) ([]string, error) {
	rows, err := s.Pool.Query(ctx,
		`insert into deliveries (event_id, endpoint_id)
		 select $1, e.id
		 from endpoints e
		 join endpoint_subscriptions es on es.endpoint_id = e.id
		 where e.application_id = $2 and es.event_type = $3 and not e.disabled
		 on conflict (event_id, endpoint_id) do nothing
		 returning id`,
		eventID, appID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// --- dispatch: delivery ---

// DeliveryJob is everything the delivery consumer needs for one HTTP attempt.
type DeliveryJob struct {
	DeliveryID   string
	Status       string
	AttemptCount int

	EndpointID string
	URL        string
	Secret     string
	RateLimit  int
	Disabled   bool

	EventID    string
	EventType  string
	Payload    json.RawMessage
	ReceivedAt time.Time
}

func (s *Store) DeliveryJob(ctx context.Context, deliveryID string) (*DeliveryJob, error) {
	var j DeliveryJob
	err := s.Pool.QueryRow(ctx,
		`select d.id, d.status, d.attempt_count,
		        ep.id, ep.url, ep.secret, ep.rate_limit_per_sec, ep.disabled,
		        ev.id, ev.event_type, ev.payload, ev.received_at
		 from deliveries d
		 join endpoints ep on ep.id = d.endpoint_id
		 join events ev on ev.id = d.event_id
		 where d.id = $1`, deliveryID).
		Scan(&j.DeliveryID, &j.Status, &j.AttemptCount,
			&j.EndpointID, &j.URL, &j.Secret, &j.RateLimit, &j.Disabled,
			&j.EventID, &j.EventType, &j.Payload, &j.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

type AttemptResult struct {
	DeliveryID string
	AttemptNo  int
	StatusCode *int
	Success    bool
	Error      *string
	DurationMS int
}

// RecordAttempt writes the attempt audit row and rolls the delivery status
// forward in one transaction. The unique (delivery_id, attempt_no) constraint
// makes redelivered attempt messages idempotent.
func (s *Store) RecordAttempt(ctx context.Context, r AttemptResult, newStatus string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`insert into delivery_attempts (delivery_id, attempt_no, status_code, success, error, duration_ms)
		 values ($1, $2, $3, $4, $5, $6)
		 on conflict (delivery_id, attempt_no) do nothing`,
		r.DeliveryID, r.AttemptNo, r.StatusCode, r.Success, r.Error, r.DurationMS)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Redelivery of an already-recorded attempt: leave state untouched.
		return tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx,
		`update deliveries
		 set status = $2, attempt_count = greatest(attempt_count, $3), updated_at = now()
		 where id = $1`,
		r.DeliveryID, newStatus, r.AttemptNo)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
