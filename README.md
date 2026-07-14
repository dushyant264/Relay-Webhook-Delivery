# Relay

Relay is a webhook delivery platform — the same kind of infrastructure Stripe
and Svix run to get webhooks from A to B reliably.

The idea: sending webhooks properly is surprisingly hard. You need retries with
backoff, payload signing, rate limiting, a way to stop hammering dead endpoints,
and an answer to "did my webhook actually arrive?". Every company that sends
webhooks ends up rebuilding all of that. Relay does it once, as a service: your
app sends an event to Relay a single time, and Relay takes care of delivering it
to every subscriber — signed, retried, and fully logged.

```
your app ──▶ Ingest (Go) ──▶ RabbitMQ ──▶ Dispatch (Go) ──▶ subscribers
                │                │
             Postgres      retry ladder / DLQ
```

Built with **Go** (the ingest and delivery hot paths), **TypeScript** (the
management API + dashboard), **RabbitMQ** (queues and a TTL-based retry ladder),
**Postgres** (source of truth and audit trail), and **Redis** (rate limits and
circuit breakers).

## Running it

You only need Docker:

```bash
git clone <repo-url> relay && cd relay
docker compose up -d --build
```

That starts everything, seeds a demo tenant, and you're live:

- **Dashboard** → http://localhost:8080 (token: `dev-admin-token`)
- **Send events** → `POST http://localhost:8081/v1/events` with the demo key
  `relay_sk_demo_c0ffee5ca1ab1efacade`
- **RabbitMQ UI** → http://localhost:15672 (`relay`/`relay`)

The quickest way to see it work is the guided demo — it sends events, shows
idempotency, and sets up a deliberately flaky endpoint so you can watch retries
climb the backoff ladder in real time:

```powershell
./scripts/demo.ps1
```

Step-by-step instructions (including every credential and where it comes from)
are in [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md).

## What's interesting about it

A few of the design problems this project deals with:

- **Exactly-once delivery doesn't exist** over HTTP, so Relay does at-least-once
  and makes duplicates harmless instead — three layers of idempotency, enforced
  by database constraints rather than application logic.
- **RabbitMQ has no native "retry in 10 minutes"**, so backoff is built from
  plain TTL queues that dead-letter back into the work queue. No plugins, no
  head-of-line blocking.
- **One bad subscriber shouldn't hurt the rest** — per-endpoint rate limits and
  a circuit breaker keep a dead or slow endpoint from eating the worker pool.
- **Every attempt is recorded** — status code, error, latency — so "where is my
  webhook?" is a query, not an investigation.
