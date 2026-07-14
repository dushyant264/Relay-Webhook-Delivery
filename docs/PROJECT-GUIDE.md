# Relay — Project Guide (full reference)

Relay is webhook infrastructure as a service (in the spirit of Svix / Stripe's
webhook system). Producers send an event to Relay **once**; Relay guarantees
signed, **at-least-once** delivery to every subscribed endpoint — with
exponential-backoff retries, per-endpoint rate limiting, circuit breaking,
dead-lettering, and a queryable audit trail.

**Stack:** Go (ingest + delivery hot paths) · TypeScript/Fastify (control plane) ·
RabbitMQ (work queues + TTL retry ladder) · Postgres (system of record) ·
Redis (rate limits, breaker state, key cache) · Prometheus/Grafana (optional)

```
Producer ──▶ Ingest (Go) ──▶ RabbitMQ ──▶ Dispatch (Go) ──signed POST──▶ Subscribers
                 │               ▲  └─ retry ladder ─┐
                 ▼               └───────────────────┘
              Postgres ◀── audit trail        Operator ──▶ Control Plane (TS)
```

## Documentation

| Doc | What's in it |
|---|---|
| [GETTING-STARTED.md](GETTING-STARTED.md) | **Clone-to-first-webhook walkthrough** — every credential explained |
| [INTERVIEW-QUESTIONS.txt](INTERVIEW-QUESTIONS.txt) | 65 interview questions about this project, with answer cues |
| [DEPLOYMENT.md](DEPLOYMENT.md) | Making it live: VPS + Caddy, or free managed tiers |
| [architecture/00-overview.md](architecture/00-overview.md) | **Whole-system HLD**, delivery lifecycle, guarantees & trade-offs |
| [architecture/01-messaging-topology.md](architecture/01-messaging-topology.md) | RabbitMQ topology, the TTL retry ladder, reliability settings |
| [architecture/02-data-model.md](architecture/02-data-model.md) | Postgres ERD and the three idempotency layers |
| [architecture/03-ingest.md](architecture/03-ingest.md) | Ingest component HLD + failure semantics |
| [architecture/04-dispatch.md](architecture/04-dispatch.md) | Dispatch component HLD: fan-out, retries, breaker, signing |
| [architecture/05-control-plane.md](architecture/05-control-plane.md) | Control-plane component HLD + API reference |
| [architecture/06-dashboard.md](architecture/06-dashboard.md) | Web dashboard component HLD |
| [PLAN.md](PLAN.md) | Build plan and milestones |

Every architecture doc leads with a hand-drawn **SVG diagram**
(`architecture/svg/`) with the mermaid source collapsible below it.

## Quick start

Prereqs: Docker Desktop (that's it — everything runs in containers).

```bash
git clone <this repo> && cd relay
docker compose up -d --build      # first build takes a few minutes
```

The Postgres container auto-applies the schema **and demo seed** on first start:
a tenant (`acme-corp`), an application (`billing-service`), three event types,
a demo API key, and an endpoint pointing at the bundled `receiver` service.

| Service | URL | Credentials |
|---|---|---|
| **Dashboard** | http://localhost:8080 | login with `dev-admin-token` |
| Ingest API | http://localhost:8081 | demo key `relay_sk_demo_c0ffee5ca1ab1efacade` |
| Control plane API | http://localhost:8080/v1 | `Authorization: Bearer dev-admin-token` |
| RabbitMQ management | http://localhost:15672 | `relay` / `relay` |
| Demo receiver | http://localhost:8090 | — |

Where these come from (and how to mint your own): [GETTING-STARTED.md](GETTING-STARTED.md).

### Send your first webhook

```bash
# bash / Git Bash / WSL (PowerShell users: see GETTING-STARTED.md for Invoke-RestMethod variants)
curl -X POST http://localhost:8081/v1/events \
  -H "Authorization: Bearer relay_sk_demo_c0ffee5ca1ab1efacade" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-001" \
  -d '{"event_type": "invoice.paid", "payload": {"invoice_id": "inv_1001", "amount_cents": 4999}}'
```

Watch it arrive (signature-verified) at the receiver:

```bash
docker compose logs -f receiver
```

Check the audit trail:

```bash
# deliveries for the event (use the event_id from the 202 response)
curl -H "Authorization: Bearer dev-admin-token" \
  http://localhost:8080/v1/events/<event_id>/deliveries

# full attempt-by-attempt drill-down
curl -H "Authorization: Bearer dev-admin-token" \
  http://localhost:8080/v1/deliveries/<delivery_id>
```

### Guided demo (happy path, idempotency, retries)

```powershell
./scripts/demo.ps1        # PowerShell (Windows)
```

To see the **retry ladder** in action, the demo creates an endpoint at
`http://receiver:8090/webhook?fail=2` — the receiver 500s the first two attempts,
so you'll see attempt 1 fail, attempt 2 fail 10s later, attempt 3 succeed ~1m
after that. `?fail=always` drives a delivery all the way to the DLQ and opens
the circuit breaker.

### Observability (optional)

```bash
docker compose --profile observability up -d
```

Prometheus at http://localhost:9090, Grafana at http://localhost:3000
(`relay`/`relay`). Interesting series: `relay_deliveries_total` by outcome,
`relay_delivery_duration_seconds`, RabbitMQ queue depths (scraped from the
broker's Prometheus plugin).

## Local development (without Docker, per service)

```bash
# infra only
docker compose up -d rabbitmq postgres redis

# Go services (each in its own terminal)
cd go && go run ./cmd/ingest      # :8081
cd go && go run ./cmd/dispatch    # :8082
cd go && go run ./cmd/receiver    # :8090  (set RECEIVER_SECRET to the endpoint secret)

# control plane
cd control-plane && npm install && npm run dev   # :8080

# tests
cd go && go test ./...
cd control-plane && npm run typecheck
```

Config is all environment variables (`DATABASE_URL`, `AMQP_URL`, `REDIS_ADDR`,
`PORT`, `ADMIN_TOKEN`, `RECEIVER_SECRET`) — see `docker-compose.yml` for the
defaults.

## Design highlights (interview cheat-sheet)

- **At-least-once + layered idempotency** instead of pretending exactly-once is
  possible over HTTP — producer idempotency keys, fan-out dedup on
  `(event_id, endpoint_id)`, attempt dedup on `(delivery_id, attempt_no)`.
- **TTL retry ladder** on stock RabbitMQ (per-queue TTL + DLX) — avoids the
  head-of-line blocking of per-message TTLs. Also reused as the parking lot for
  rate-limited and breaker-open deliveries (attempt-free).
- **Stripe-style HMAC signatures** with timestamp-based replay protection.
- **Per-endpoint isolation**: token-per-second rate limiting and a Redis-backed
  circuit breaker (5 consecutive failures → 2-minute quarantine, half-open via
  key TTL). Redis fails open: it can degrade protection, never correctness.
- **Crash-only recovery**: services exit on broker connection loss and rely on
  the orchestrator to restart them; all state is in Postgres/RabbitMQ.
