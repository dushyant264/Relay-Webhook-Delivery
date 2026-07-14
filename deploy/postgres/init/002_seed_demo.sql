-- Demo seed so the stack is usable immediately after `docker compose up`.
-- DEMO CREDENTIALS — local development only.
--   API key (send events):  relay_sk_demo_c0ffee5ca1ab1efacade
--   Endpoint secret (HMAC): whsec_demo_5f4dcc3b5aa765d61d83  (receiver env matches)

with t as (
    insert into tenants (id, name)
    values ('11111111-1111-1111-1111-111111111111', 'acme-corp')
    returning id
), a as (
    insert into applications (id, tenant_id, name)
    select '22222222-2222-2222-2222-222222222222', id, 'billing-service' from t
    returning id
), k as (
    insert into api_keys (application_id, prefix, key_hash)
    select id,
           'relay_sk_demo_c0',
           encode(digest('relay_sk_demo_c0ffee5ca1ab1efacade', 'sha256'), 'hex')
    from a
), et as (
    insert into event_types (application_id, name, description)
    select id, x.name, x.descr
    from a, (values
        ('invoice.paid',    'An invoice was paid in full'),
        ('invoice.overdue', 'An invoice passed its due date unpaid'),
        ('customer.created','A new customer signed up')
    ) as x(name, descr)
), e as (
    insert into endpoints (id, application_id, url, secret, description, rate_limit_per_sec)
    select '33333333-3333-3333-3333-333333333333', id,
           'http://receiver:8090/webhook',
           'whsec_demo_5f4dcc3b5aa765d61d83',
           'local demo receiver', 5
    from a
    returning id
)
insert into endpoint_subscriptions (endpoint_id, event_type)
select id, x.event_type
from e, (values ('invoice.paid'), ('invoice.overdue')) as x(event_type);
