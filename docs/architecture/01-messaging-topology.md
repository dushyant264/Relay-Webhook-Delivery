# RabbitMQ Messaging Topology — Component HLD

**Code:** [`go/internal/mq/topology.go`](../../go/internal/mq/topology.go)

Every service declares the full topology idempotently at startup, so no manual
broker setup is needed and start order doesn't matter.

## Diagram

![RabbitMQ topology and retry ladder](svg/01-messaging-topology.svg)

<details><summary>Mermaid source</summary>

```mermaid
flowchart LR
    IN[Ingest] -->|event.received| EX1{{relay.events\ndirect}}
    EX1 --> Q1[(relay.event-fanout)]
    Q1 --> FO[Dispatch: fan-out consumer]

    FO -->|delivery| EX2{{relay.deliveries\ndirect}}
    EX2 --> Q2[(relay.deliveries)]
    Q2 --> DL[Dispatch: delivery consumer]

    DL -->|"failed attempt n →\ntier min(n-1, 4)"| EX3{{relay.retry\ndirect}}
    EX3 -->|10s| R1[(relay.retry.10s\nTTL 10s)]
    EX3 -->|1m| R2[(relay.retry.1m\nTTL 1m)]
    EX3 -->|5m| R3[(relay.retry.5m\nTTL 5m)]
    EX3 -->|30m| R4[(relay.retry.30m\nTTL 30m)]
    EX3 -->|2h| R5[(relay.retry.2h\nTTL 2h)]
    R1 & R2 & R3 & R4 & R5 -.->|"TTL expiry dead-letters\nback to relay.deliveries"| EX2

    DL -->|"attempts exhausted (6)"| EX4{{relay.dlx\ndirect}}
    EX4 --> DLQ[(relay.dlq)]
```

</details>

## The retry ladder (why it looks like this)

**Goal:** exponential backoff (10s → 1m → 5m → 30m → 2h) on stock RabbitMQ.

RabbitMQ has no native "redeliver in N seconds". The two standard options:

1. **Per-message TTL** in one retry queue — broken by design: RabbitMQ only
   expires the message at the *head* of a queue, so a 2h message parked in front
   of a 10s message blocks it (head-of-line blocking).
2. **Per-queue TTL ladder** (chosen) — one queue per delay tier, each declared
   with `x-message-ttl` and `x-dead-letter-exchange` pointing back at
   `relay.deliveries`. Every message in a queue has the same TTL, so expiry
   order == arrival order and nothing blocks.

A failed attempt `n` is published to tier `min(n-1, 4)`; with `MaxAttempts = 6`
a delivery's worst-case schedule is: try, +10s, +1m, +5m, +30m, +2h → DLQ.

The ladder is also reused as a **parking lot**: rate-limited deliveries park in
`10s`, breaker-open deliveries park in `1m` — in both cases *without* consuming
an attempt.

## Reliability settings

- **Durable everything** — exchanges, queues, and `DeliveryMode: Persistent`
  messages survive a broker restart.
- **Publisher confirms** — every publish waits for the broker ack; ingest returns
  `202` and consumers ack their input message only after downstream publishes are
  confirmed.
- **Manual acks + prefetch 16** — a crashed worker's unacked messages are
  redelivered (at-least-once); prefetch bounds memory and spreads work across
  worker replicas.
- **Messages carry ids, not payloads** — the payload lives in Postgres; a queue
  wipe loses schedule state, not data.
- **Poison messages** (unparseable JSON) are logged and acked, never requeued —
  a parse error can't succeed on retry, and requeueing would hot-loop the queue.
