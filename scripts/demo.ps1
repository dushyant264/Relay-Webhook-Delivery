# Relay end-to-end demo (uses the seeded demo tenant/app/endpoint).
# Prereq: docker compose up -d --build   (all services healthy)
$ErrorActionPreference = "Stop"

$apiKey = "relay_sk_demo_c0ffee5ca1ab1efacade"
$ingest = "http://localhost:8081"
$cp     = "http://localhost:8080"
$admin  = @{ Authorization = "Bearer dev-admin-token" }

Write-Host "`n=== 1. Send an event (happy path) ===" -ForegroundColor Cyan
$resp = Invoke-RestMethod -Method Post -Uri "$ingest/v1/events" `
    -Headers @{ Authorization = "Bearer $apiKey"; "Idempotency-Key" = [guid]::NewGuid() } `
    -ContentType "application/json" `
    -Body (@{ event_type = "invoice.paid"; payload = @{ invoice_id = "inv_1001"; amount_cents = 4999 } } | ConvertTo-Json)
$eventId = $resp.event_id
Write-Host "Accepted: event_id = $eventId"

Start-Sleep -Seconds 3

Write-Host "`n=== 2. Delivery status for that event ===" -ForegroundColor Cyan
Invoke-RestMethod -Uri "$cp/v1/events/$eventId/deliveries" -Headers $admin | Format-Table -AutoSize

Write-Host "=== 3. Idempotency: resend with the SAME Idempotency-Key ===" -ForegroundColor Cyan
$key = [guid]::NewGuid().ToString()
$body = @{ event_type = "invoice.paid"; payload = @{ invoice_id = "inv_1002"; amount_cents = 100 } } | ConvertTo-Json
$first  = Invoke-RestMethod -Method Post -Uri "$ingest/v1/events" -Headers @{ Authorization = "Bearer $apiKey"; "Idempotency-Key" = $key } -ContentType "application/json" -Body $body
$second = Invoke-RestMethod -Method Post -Uri "$ingest/v1/events" -Headers @{ Authorization = "Bearer $apiKey"; "Idempotency-Key" = $key } -ContentType "application/json" -Body $body
Write-Host "first:  event_id=$($first.event_id)"
Write-Host "second: event_id=$($second.event_id) duplicate=$($second.duplicate)  <- same id, no double delivery"

Write-Host "`n=== 4. Retry path: create a FLAKY endpoint (fails first 2 attempts) ===" -ForegroundColor Cyan
$flaky = Invoke-RestMethod -Method Post -Uri "$cp/v1/applications/22222222-2222-2222-2222-222222222222/endpoints" `
    -Headers $admin -ContentType "application/json" `
    -Body (@{ url = "http://receiver:8090/webhook?fail=2"; description = "flaky demo endpoint"; event_types = @("invoice.overdue"); secret = "whsec_demo_5f4dcc3b5aa765d61d83" } | ConvertTo-Json)
Write-Host "Created flaky endpoint $($flaky.id) (secret shown once: $($flaky.secret))"
Write-Host "NOTE: its receiver will 500 twice; watch attempts land 10s then 1m apart."

$resp = Invoke-RestMethod -Method Post -Uri "$ingest/v1/events" `
    -Headers @{ Authorization = "Bearer $apiKey" } -ContentType "application/json" `
    -Body (@{ event_type = "invoice.overdue"; payload = @{ invoice_id = "inv_1001"; days_overdue = 14 } } | ConvertTo-Json)
Write-Host "Sent invoice.overdue: event_id = $($resp.event_id)"
Write-Host "Poll it:  Invoke-RestMethod -Uri $cp/v1/events/$($resp.event_id)/deliveries -Headers @{Authorization='Bearer dev-admin-token'}"

Write-Host "`n=== 5. Where to look ===" -ForegroundColor Cyan
Write-Host " docker compose logs -f dispatch receiver   # signed deliveries, retries, breaker"
Write-Host " http://localhost:15672 (relay/relay)       # RabbitMQ: watch the retry ladder queues"
Write-Host " GET $cp/v1/deliveries/<delivery_id>        # full attempt audit trail"
