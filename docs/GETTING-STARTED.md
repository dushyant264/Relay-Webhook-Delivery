# Getting Started — from `git clone` to your first webhook

This is the complete walkthrough for someone who just downloaded the repo and
knows nothing about it. Total time: ~10 minutes, most of it Docker building.

## 0. Prerequisites

| Tool | Why | Check |
|---|---|---|
| **Docker Desktop** (with Compose v2) | everything runs in containers | `docker compose version` |
| *(optional)* Go ≥ 1.26, Node ≥ 20 | only if you want to run services outside Docker or hack on the code | `go version`, `node -v` |

On Windows, Docker Desktop needs WSL2 (its installer sets that up).

## 1. Start everything

```bash
git clone <repo-url> relay && cd relay
docker compose up -d --build     # ~3-5 min the first time
docker compose ps                # wait until postgres/rabbitmq/redis say "healthy"
```

That single command starts 7 containers: RabbitMQ, Postgres, Redis, and the four
Relay services (ingest, dispatch, control-plane, demo receiver).

## 2. Where the credentials come from (READ THIS)

There is no signup flow — on **first start**, Postgres auto-applies
[`deploy/postgres/init/001_schema.sql`](../deploy/postgres/init/001_schema.sql)
(the schema) and [`002_seed_demo.sql`](../deploy/postgres/init/002_seed_demo.sql)
(a demo tenant so you can play immediately). Everything below is **local
demo-grade** and lives in those two files + `docker-compose.yml`:

| Credential | Value | Used for | Defined in |
|---|---|---|---|
| **Producer API key** | `relay_sk_demo_c0ffee5ca1ab1efacade` | `Authorization: Bearer …` on the **ingest** API (send events) | seeded (hashed) in `002_seed_demo.sql` |
| **Admin token** | `dev-admin-token` | `Authorization: Bearer …` on the **control-plane** API + dashboard login | `ADMIN_TOKEN` env in `docker-compose.yml` |
| **Endpoint HMAC secret** | `whsec_demo_5f4dcc3b5aa765d61d83` | receiver verifies `Relay-Signature` | seed + `RECEIVER_SECRET` env |
| RabbitMQ UI | `relay` / `relay` | http://localhost:15672 | compose env |
| Postgres | `relay` / `relay`, db `relay` | `localhost:5432` | compose env |

Seeded demo objects (fixed UUIDs so the docs can reference them):
- Tenant `acme-corp` → application **`billing-service`** (`22222222-2222-2222-2222-222222222222`)
- Event types: `invoice.paid`, `invoice.overdue`, `customer.created`
- One endpoint → the bundled receiver, subscribed to `invoice.paid` + `invoice.overdue`

## 3. Open the dashboard

**http://localhost:8080** → paste the admin token `dev-admin-token` top-right →
you'll see stat tiles, the live delivery feed and configured endpoints.

## 4. Send your first event

> **Windows note:** in PowerShell, `curl` is an alias for `Invoke-WebRequest` and
> `\` continuations don't work. Use the PowerShell variants below, or call real
> curl as `curl.exe` on a single line.

**bash / Git Bash / WSL:**

```bash
curl -X POST http://localhost:8081/v1/events \
  -H "Authorization: Bearer relay_sk_demo_c0ffee5ca1ab1efacade" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: my-first-event" \
  -d '{"event_type": "invoice.paid", "payload": {"invoice_id": "inv_1", "amount_cents": 999}}'
```

**PowerShell:**

```powershell
Invoke-RestMethod -Method Post -Uri "http://localhost:8081/v1/events" `
  -Headers @{ Authorization = "Bearer relay_sk_demo_c0ffee5ca1ab1efacade"; "Idempotency-Key" = "my-first-event" } `
  -ContentType "application/json" `
  -Body '{"event_type":"invoice.paid","payload":{"invoice_id":"inv_1","amount_cents":999}}'
```

Within a second: the dashboard feed shows a `succeeded` delivery, and
`docker compose logs receiver` shows `webhook received ✓ signature verified`.
Click the delivery row in the dashboard for the attempt-by-attempt audit trail.

## 5. See the retry ladder (the fun part)

```powershell
./scripts/demo.ps1     # PowerShell — full guided tour
```

or manually: create an endpoint whose receiver fails the first 2 attempts
(`?fail=2`), send `invoice.overdue`, and watch attempts land at +0s (fail),
+10s (fail), +70s (success) in the dashboard drill-down. `?fail=always` drives
a delivery to the dead-letter queue after 6 attempts.

## 6. Create your OWN tenant/app/key (instead of the seed)

**bash / Git Bash / WSL:**

```bash
CP=http://localhost:8080; AUTH='Authorization: Bearer dev-admin-token'

curl -X POST $CP/v1/tenants -H "$AUTH" -H "Content-Type: application/json" -d '{"name":"my-company"}'
curl -X POST $CP/v1/applications -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"tenant_id":"<tenant id from above>","name":"my-app"}'
curl -X POST $CP/v1/applications/<app-id>/event-types -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"name":"order.created"}'
curl -X POST $CP/v1/applications/<app-id>/endpoints -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"url":"https://your-server/webhook","event_types":["order.created"]}'
# → response contains the endpoint HMAC "secret" — shown ONCE, configure it on your receiver
curl -X POST $CP/v1/applications/<app-id>/api-keys -H "$AUTH"
# → response contains "key": "relay_sk_..." — shown ONCE, this is your producer key
```

**PowerShell:**

```powershell
$CP = "http://localhost:8080"; $AUTH = @{ Authorization = "Bearer dev-admin-token" }

$tenant = Invoke-RestMethod -Method Post -Uri "$CP/v1/tenants" -Headers $AUTH -ContentType "application/json" -Body '{"name":"my-company"}'
$app = Invoke-RestMethod -Method Post -Uri "$CP/v1/applications" -Headers $AUTH -ContentType "application/json" -Body (@{ tenant_id = $tenant.id; name = "my-app" } | ConvertTo-Json)
Invoke-RestMethod -Method Post -Uri "$CP/v1/applications/$($app.id)/event-types" -Headers $AUTH -ContentType "application/json" -Body '{"name":"order.created"}'
$endpoint = Invoke-RestMethod -Method Post -Uri "$CP/v1/applications/$($app.id)/endpoints" -Headers $AUTH -ContentType "application/json" -Body '{"url":"https://your-server/webhook","event_types":["order.created"]}'
$endpoint.secret   # endpoint HMAC secret — shown ONCE, configure it on your receiver
$key = Invoke-RestMethod -Method Post -Uri "$CP/v1/applications/$($app.id)/api-keys" -Headers $AUTH
$key.key           # your producer API key — shown ONCE
```

## 7. Everyday commands

```bash
docker compose logs -f dispatch receiver   # watch deliveries and retries live
docker compose ps                          # health
docker compose down                        # stop (keeps data volumes)
docker compose down -v                     # stop and WIPE data (seed re-applies next start)
docker compose --profile observability up -d   # + Prometheus (9090) & Grafana (3000, relay/relay)
```

## Troubleshooting

- **`docker compose` not found / engine not running** → start Docker Desktop, wait for the whale icon.
- **Port already in use** → something on 8080/8081/5432/5672; change the left side of the port mapping in `docker-compose.yml`.
- **401 from ingest** → key must start `relay_sk_` and match a seeded/issued key; check the `Authorization: Bearer` header.
- **Deliveries stuck `pending`** → is `dispatch` running? `docker compose logs dispatch`.
- **Schema changes don't apply** → init SQL only runs on a fresh volume: `docker compose down -v && docker compose up -d --build`.
