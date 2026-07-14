# Making Relay Live (public deployment)

Local `docker compose up` is the dev story. To put Relay on the public internet
(a live demo URL on your resume is worth a lot), you have three realistic paths,
cheapest-effort first.

## Option A — One VPS with Docker Compose (recommended, ~$4–6/mo)

The whole stack already runs under compose, so a single small VM is the least
moving parts. Providers: Hetzner (CX22), DigitalOcean, Oracle Cloud free tier.

1. Create an Ubuntu VM, install Docker (`curl -fsSL https://get.docker.com | sh`).
2. `git clone` your repo on the VM, then `docker compose up -d --build`.
3. **Change every demo credential first** (this is the non-negotiable step):
   - `ADMIN_TOKEN` in `docker-compose.yml` → long random string
   - RabbitMQ + Postgres passwords in compose env
   - Delete the demo seed (`deploy/postgres/init/002_seed_demo.sql`) and create
     your tenant/app/keys through the control-plane API instead
4. Put **Caddy** (2-line config, automatic HTTPS) or nginx in front:
   - `relay.yourdomain.dev` → control-plane :8080 (dashboard + API)
   - `ingest.yourdomain.dev` → ingest :8081
   - Do NOT expose 5432/5672/6379/15672 publicly (bind them to 127.0.0.1 or
     remove the port mappings — services talk over the compose network).
5. Point DNS at the VM. Done — `https://relay.yourdomain.dev` is your live demo.

## Option B — Managed platforms, free tiers (no VM to babysit)

Split the stack across free managed services:

| Piece | Free option |
|---|---|
| Go services + control plane | Fly.io / Railway / Render (deploy each Dockerfile; the Go image has 3 targets — set `--build-arg`/target per app) |
| Postgres | Neon or Supabase free tier |
| RabbitMQ | CloudAMQP "Little Lemur" free plan |
| Redis | Upstash free tier |

Set `DATABASE_URL`, `AMQP_URL`, `REDIS_ADDR`, `ADMIN_TOKEN` env vars on each
service to the managed endpoints. Run the schema once against Neon:
`psql "$DATABASE_URL" -f deploy/postgres/init/001_schema.sql`.

Caveats: free tiers sleep/limit connections; CloudAMQP free caps queue length —
fine for a demo, document it. This option is more config, but $0.

## Option C — Kubernetes

Only worth it if you want K8s itself on the resume. The services are already
12-factor (env config, stateless, crash-only, /healthz), so the port is
mechanical: one Deployment + Service per component, managed Postgres/RabbitMQ,
an Ingress for control-plane and ingest. Good v2 project; don't start here.

## Production checklist (whatever the option)

- [ ] Rotate `ADMIN_TOKEN`, broker/db passwords; remove the demo seed
- [ ] HTTPS termination in front of ingest + control plane
- [ ] Never expose RabbitMQ/Postgres/Redis ports publicly
- [ ] Keep the `observability` profile (or platform metrics) running
- [ ] Backups: Postgres volume snapshot or managed-DB PITR (it's the system of record;
      RabbitMQ/Redis are rebuildable)

## What to put on the resume once it's live

> *Deployed a multi-service webhook delivery platform (Go, TypeScript, RabbitMQ,
> Postgres, Redis) behind HTTPS on a single VPS with Docker Compose; live demo at
> relay.yourdomain.dev — signed at-least-once delivery with exponential backoff,
> circuit breaking and a full audit trail.*
