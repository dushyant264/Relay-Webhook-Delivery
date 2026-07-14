-- Relay schema. Applied automatically by the postgres container on first start.
create extension if not exists pgcrypto;

create table tenants (
    id         uuid primary key default gen_random_uuid(),
    name       text not null unique,
    created_at timestamptz not null default now()
);

create table applications (
    id         uuid primary key default gen_random_uuid(),
    tenant_id  uuid not null references tenants(id) on delete cascade,
    name       text not null,
    created_at timestamptz not null default now(),
    unique (tenant_id, name)
);

-- Producer credentials. Only a sha256 hash is stored; the raw key is shown once
-- at creation. Lookup is by unique prefix, then constant-time hash comparison.
create table api_keys (
    id             uuid primary key default gen_random_uuid(),
    application_id uuid not null references applications(id) on delete cascade,
    prefix         text not null unique,
    key_hash       text not null,
    created_at     timestamptz not null default now(),
    revoked_at     timestamptz
);

create table event_types (
    id             uuid primary key default gen_random_uuid(),
    application_id uuid not null references applications(id) on delete cascade,
    name           text not null,
    description    text,
    created_at     timestamptz not null default now(),
    unique (application_id, name)
);

create table endpoints (
    id                 uuid primary key default gen_random_uuid(),
    application_id     uuid not null references applications(id) on delete cascade,
    url                text not null,
    secret             text not null,
    description        text,
    rate_limit_per_sec integer not null default 5 check (rate_limit_per_sec > 0),
    disabled           boolean not null default false,
    created_at         timestamptz not null default now()
);

create table endpoint_subscriptions (
    endpoint_id uuid not null references endpoints(id) on delete cascade,
    event_type  text not null,
    primary key (endpoint_id, event_type)
);

create table events (
    id              uuid primary key default gen_random_uuid(),
    application_id  uuid not null references applications(id) on delete cascade,
    event_type      text not null,
    payload         jsonb not null,
    idempotency_key text,
    received_at     timestamptz not null default now()
);
-- Producer-side dedup: retrying the same Idempotency-Key must not create a new event.
create unique index events_idem_uq
    on events (application_id, idempotency_key) where idempotency_key is not null;
create index events_app_type_idx on events (application_id, event_type, received_at desc);

create table deliveries (
    id            uuid primary key default gen_random_uuid(),
    event_id      uuid not null references events(id) on delete cascade,
    endpoint_id   uuid not null references endpoints(id) on delete cascade,
    status        text not null default 'pending'
                  check (status in ('pending', 'succeeded', 'failed', 'dead')),
    attempt_count integer not null default 0,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now()
);
-- Fan-out idempotency: redelivered event messages must not duplicate deliveries.
create unique index deliveries_event_endpoint_uq on deliveries (event_id, endpoint_id);
create index deliveries_endpoint_idx on deliveries (endpoint_id, created_at desc);
create index deliveries_event_idx on deliveries (event_id);

create table delivery_attempts (
    id          bigserial primary key,
    delivery_id uuid not null references deliveries(id) on delete cascade,
    attempt_no  integer not null,
    status_code integer,
    success     boolean not null,
    error       text,
    duration_ms integer,
    created_at  timestamptz not null default now(),
    unique (delivery_id, attempt_no)
);
