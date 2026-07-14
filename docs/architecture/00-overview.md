# Relay — System Overview (HLD)

Relay is a multi-tenant **webhook delivery platform**. A producer emits an event to
Relay **once**; Relay takes on the hard parts of webhook infrastructure — fan-out,
signing, retries with exponential backoff, rate limiting, circuit breaking, and a
queryable audit trail — and guarantees **at-least-once** delivery to every
subscribed endpoint.

## High-level diagram

![Relay system overview](svg/00-overview.svg)

<details><summary>Mermaid source (renders on GitHub)</summary>

```mermaid
flowchart LR
    subgraph clients [Producers & Operators]
        P[Producer service]
        OP[Operator / Dashboard]
    end

    subgraph relay [Relay]
        direction LR
        CP["Control Plane (TypeScript · Fastify)\ntenants · apps · endpoints · keys · logs"]
        IN["Ingest (Go)\nauth · validate · persist · publish"]
        subgraph mq [RabbitMQ]
            EF[(event-fanout q)]
            DQ[(deliveries q)]
            RL[(retry ladder\n10s→1m→5m→30m→2h)]
            DLQ[(dead-letter q)]
        end
        DI["Dispatch (Go)\nfan-out · sign · POST · retry"]
        PG[(Postgres\nsystem of record)]
        RD[(Redis\nrate limits · breaker · key cache)]
    end

    S1[Subscriber endpoint A]
    S2[Subscriber endpoint B]

    P -- "POST /v1/events\n(API key + Idempotency-Key)" --> IN
    OP -- "manage config, query logs" --> CP
    IN -- "1. insert event" --> PG
    IN -- "2. publish (confirmed)" --> EF
    EF --> DI
    DI -- "resolve subscriptions,\ncreate delivery rows" --> PG
    DI -- "1 msg per endpoint" --> DQ
    DQ --> DI
    DI -- "HMAC-signed POST" --> S1
    DI -- "HMAC-signed POST" --> S2
    DI -- "on failure → backoff tier" --> RL
    RL -- "TTL expiry re-queues" --> DQ
    DI -- "attempts exhausted" --> DLQ
    DI <--> RD
    CP --> PG
```

</details>

## Components

| Component | Stack | Role |
|---|---|---|
| **Ingest** | Go, `net/http`, pgx, amqp091 | The hot path. Authenticates API keys (Redis-cached), validates the event, persists it idempotently, publishes to RabbitMQ with publisher confirms, returns `202` with the event id. |
| **Dispatch** | Go | Two consumer pools in one service. **Fan-out**: one event message → N durable `delivery` rows + N queue messages. **Delivery**: rate-limit + breaker checks, HMAC signing, HTTP POST, attempt recording, retry-ladder routing. |
| **Control plane** | TypeScript, Fastify, zod, pg | Product surface: CRUD for tenants → applications → endpoints/event types, API-key issuance (hashed at rest, shown once), delivery-log query API. |
| **RabbitMQ** | topic/direct exchanges, quorum-style durable queues | Work distribution and the retry ladder (below). |
| **Postgres** | 16 | System of record: config, events, deliveries, attempts. See [02-data-model.md](02-data-model.md). |
| **Redis** | 7 | Token-bucket rate limit per endpoint, circuit-breaker state, API-key cache. Ephemeral by design — losing it degrades performance, not correctness. |

Per-component deep dives: [ingest](03-ingest.md) · [dispatch](04-dispatch.md) ·
[control plane](05-control-plane.md) · [messaging topology](01-messaging-topology.md)

## Delivery lifecycle

```mermaid
sequenceDiagram
    participant Pr as Producer
    participant In as Ingest (Go)
    participant PG as Postgres
    participant MQ as RabbitMQ
    participant Di as Dispatch (Go)
    participant Ep as Subscriber

    Pr->>In: POST /v1/events (API key, Idempotency-Key)
    In->>PG: INSERT event (idempotent upsert)
    In->>MQ: publish event.received (confirmed)
    In-->>Pr: 202 {event_id}
    MQ->>Di: event-fanout consumer
    Di->>PG: resolve subscribed endpoints, INSERT deliveries
    Di->>MQ: publish N delivery msgs
    MQ->>Di: deliveries consumer
    Di->>Di: rate limit? breaker open?
    Di->>Ep: POST payload + Relay-Signature
    alt 2xx
        Di->>PG: attempt: succeeded
    else failure
        Di->>PG: attempt: failed (status, error, latency)
        Di->>MQ: publish to retry tier N (TTL queue)
        MQ-->>MQ: TTL expiry dead-letters back to deliveries q
    end
```

## Guarantees & trade-offs

- **At-least-once, not exactly-once.** The insert-then-publish window means a crash
  between the two leaves a persisted event that was never published; the producer's
  retry (same `Idempotency-Key`) resolves it. Duplicate deliveries are possible
  (e.g. worker crash after POST, before ack) — subscribers deduplicate on
  `Relay-Id`. Exactly-once over HTTP is not achievable; we make duplicates safe
  instead of pretending they can't happen.
- **Ordering is not guaranteed** across retries by design (a failed delivery must
  not block subsequent events — no head-of-line blocking). Subscribers needing
  order use the embedded `timestamp`.
- **Isolation between tenants/endpoints**: a dead or slow endpoint affects only
  itself — rate limiting bounds throughput per endpoint, the breaker stops wasted
  work, and the retry ladder keeps failing traffic off the main queue.
- **Redis is a soft dependency**: rate limits/breaker fail open (with a log) if
  Redis is down; Postgres and RabbitMQ are hard dependencies.

## Security model

- Producer auth: per-application API keys, `sha256`-hashed at rest, prefix-indexed.
- Delivery auth: `Relay-Signature: v1=<hex HMAC-SHA256(endpoint_secret, "{id}.{ts}.{body}")>`
  plus `Relay-Timestamp` — subscribers verify both the signature and timestamp
  freshness (replay protection).
- Control plane: admin bearer token (demo-grade; SSO/JWT is a v2 concern).
