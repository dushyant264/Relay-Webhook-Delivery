# Relay — Build Plan

> **Relay** is a webhook delivery platform (in the spirit of Svix / Stripe's webhook
> infrastructure). Producers send events to Relay once; Relay guarantees signed,
> at-least-once delivery to every subscribed endpoint, with retries, rate limiting,
> circuit breaking and a full audit trail.

## 1. Goals

- **Backend-heavy, interview-ready**: every feature maps to a system-design topic
  (delivery guarantees, retry strategies, idempotency, back-pressure, multi-tenancy).
- **Polyglot by design**: Go where throughput matters (ingest, delivery), TypeScript
  where product/API surface matters (control plane).
- **Honest distributed-systems trade-offs**, documented in `docs/architecture/`.

### Non-goals (v1)
- Exactly-once delivery (we do at-least-once + idempotency keys and document why).
- A polished frontend (control plane is API-first; a UI can come later).
- Kubernetes (docker-compose is the deployment story for v1).

## 2. System summary

```
Producer ──POST /v1/events──▶ Ingest (Go) ──▶ RabbitMQ ──▶ Dispatch (Go) ──HTTP──▶ Subscriber endpoints
                                   │                            │
                                   ▼                            ▼
                               Postgres ◀──────────────── delivery audit trail
                                   ▲
Operator ──manage tenants/apps/endpoints──▶ Control Plane (TypeScript)
```

| Component | Language | Responsibility |
|---|---|---|
| **Ingest** | Go | High-throughput event intake: API-key auth, validation, idempotent persist, publish to RabbitMQ |
| **Dispatch** | Go | Two consumers: *fan-out* (event → N deliveries) and *delivery* (sign + POST + retry/breaker) |
| **Control plane** | TypeScript (Fastify) | CRUD for tenants, applications, endpoints, event types, API keys; delivery-log query API |
| **Receiver** | Go | Demo subscriber: verifies signatures, can simulate failures to show off retries |
| **RabbitMQ** | — | Work queues, TTL-based retry ladder, dead-letter queue |
| **Postgres** | — | System of record: config + events + delivery attempts |
| **Redis** | — | Per-endpoint rate limiting, circuit-breaker state, API-key cache |
| **Prometheus/Grafana** | — | Metrics (optional compose profile `observability`) |

Full architecture: [architecture/00-overview.md](architecture/00-overview.md)

## 3. Key design decisions

1. **At-least-once, not exactly-once.** Event is persisted to Postgres first, then
   published with publisher confirms. If the publish fails the client gets a 5xx and
   retries with the same `Idempotency-Key` (unique per application). Consumers are
   idempotent. Trade-off documented in the overview doc.
2. **Retry ladder instead of a delay plugin.** Stock RabbitMQ has no native delayed
   messages. We build an exponential-backoff ladder from per-queue TTLs +
   dead-letter exchanges (`retry.10s → retry.1m → retry.5m → retry.30m → retry.2h`),
   which avoids the head-of-line blocking of per-message TTLs.
3. **Fan-out as a separate stage.** Ingest publishes ONE message per event; a fan-out
   consumer resolves subscriptions and creates one durable `delivery` row + queue
   message per endpoint. Keeps ingest fast and makes fan-out horizontally scalable.
4. **Signed payloads.** Every delivery carries `Relay-Signature: v1=HMAC-SHA256(secret,
   "{id}.{timestamp}.{body}")` + timestamp for replay protection — the Stripe scheme.
5. **Circuit breaker per endpoint.** N consecutive failures opens the breaker
   (Redis key with TTL); while open, deliveries skip the HTTP call and go straight to
   a long retry tier. Protects workers from dead endpoints.
6. **Per-endpoint rate limiting** in Redis, so one slow subscriber
   can't be hammered (or hog workers).

## 4. Milestones & steps

### M0 — Skeleton & infrastructure
- [x] Repo scaffold, git init, plan + overview HLD
- [x] `docker-compose.yml`: RabbitMQ (management), Postgres 16, Redis 7, all services, observability profile
- [x] Postgres schema (`deploy/postgres/init/001_schema.sql`) + seed script
- [x] Data-model doc (`architecture/02-data-model.md`)

### M1 — Ingest service (Go)
- [x] Go module, config loader, structured logging, graceful shutdown
- [x] RabbitMQ topology declaration (exchanges/queues/ladder) shared package
- [x] `POST /v1/events`: bearer API key (Redis-cached), validation, idempotent insert, confirmed publish
- [x] `/healthz`, `/metrics`
- [x] Component HLD doc (`architecture/03-ingest.md`)

### M2 — Dispatch service (Go)
- [x] Fan-out consumer: event → subscription lookup → delivery rows + messages
- [x] Delivery consumer: rate-limit check → breaker check → sign → POST → record attempt
- [x] Retry ladder routing + DLQ after max attempts
- [x] Circuit breaker + rate limiter (Redis)
- [x] Component HLD doc (`architecture/04-dispatch.md`) + messaging topology doc (`architecture/01-messaging-topology.md`)

### M3 — Control plane (TypeScript)
- [x] Fastify + zod + pg, layered (routes → SQL layer)
- [x] CRUD: tenants, applications, event types, endpoints (+subscriptions), API keys (hashed, shown once)
- [x] Delivery-log API: list deliveries/attempts per event or endpoint
- [x] Component HLD doc (`architecture/05-control-plane.md`)

### M4 — Demo & polish
- [x] Receiver service (signature verification, `?fail=N` failure simulation)
- [x] `scripts/demo.ps1` — end-to-end walkthrough (seed → send event → watch retries)
- [x] Prometheus scrape config, metrics on all services
- [x] README: run steps, API reference, demo walkthrough

### M5 — End-to-end verification
- [x] `docker compose up`, send event, verify happy path (signed delivery, attempt 1)
- [x] Idempotency: duplicate `Idempotency-Key` returns original event id, no re-delivery
- [x] Retry path: flaky endpoint fails twice, succeeds on attempt 3 via 10s → 1m tiers

## 5. Risks / notes
- Docker Desktop required to run infra locally (WSL2 on Windows).
- RabbitMQ topology is declared idempotently by services at startup — no manual setup.
- Secrets in compose are demo-grade; documented as such.
