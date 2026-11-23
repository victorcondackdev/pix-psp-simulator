# pix-psp-simulator

[![ci](https://github.com/victorcondackdev/pix-psp-simulator/actions/workflows/ci.yml/badge.svg)](https://github.com/victorcondackdev/pix-psp-simulator/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/victorcondackdev/pix-psp-simulator.svg)](https://pkg.go.dev/github.com/victorcondackdev/pix-psp-simulator)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A local sandbox that plays the parts of a Pix PSP, the SPI and the DICT, so a
team can build and test a Pix integration offline: instant transfers, key
lookups, QR charges, signed webhooks, devoluções, MED, and the failures that
production will eventually throw at you, on demand.

One static binary, no database, no external services. Start it, seed it with a
scenario file, point your service at it, and make it misbehave.

## Why

Integrating with Pix means integrating with asynchronous, failure-prone
infrastructure. The interesting bugs are not in the happy path:

- a transfer is accepted and then rejected seconds later with `AM04`;
- the SPI times out (`AB03`) after your request already returned;
- the same webhook arrives twice, or `pix.settled` arrives before your own
  database committed the payment;
- a dynamic QR code expires while the customer is still on the payment screen;
- a MED infraction blocks funds you already considered yours.

Provider sandboxes rarely let you trigger these deterministically, are often
rate limited or down, and cannot be run in CI. This simulator makes every one
of those cases a header or a line in a JSON file.

It is built on [brpay](https://github.com/victorcondackdev/brpay) for the
parts that must be exactly right: Pix key normalization (including the
alphanumeric CNPJ), BR Code generation, decoding and linting, EndToEndId
generation and document masking.

## Features

| Area | What you get |
|------|--------------|
| DICT | Register, delete and look up CPF, CNPJ (numeric and alphanumeric), e-mail, phone and EVP keys; masked owner document (`***.982.247-**`); 5/20 keys per account; ownership and portability claims with a resolution period; per-payer token-bucket rate limiting with a heavier cost for missing keys |
| Charges (`/cob`) | Static and dynamic BR Codes; dynamic payload served at `/qr/v2/{id}`; expiration; single-use dynamic charges |
| Payments | By key, by account details (manual), by account ID, or by "copia e cola" (decoded and linted; lint errors come back as structured issues) |
| Settlement | Asynchronous, configurable latency, `ACSP` then `ACSC` or `RJCT` with ISO 20022 reason codes, on a double-entry in-memory ledger (int64 cents) |
| Devoluções | Full or partial, multiple per transfer, up to the original amount (`BE08`, `MD06`, `SL02`) |
| MED 2.0 | Infraction report, cautionary block that follows the money through up to 5 accounts, analysis, refund (`FR01`) or release, block that lapses after 72h, as an explicit state machine; `med_version: "1.0"` keeps the payee-only block |
| Regulator (STA) | Receives CADOC 4111 (Saldos Contábeis Diários) and 4010 (Balancete) XML, validates layout, business-day data-base, I/S replacement, sequence and deadline, reconciles balances and Pix volume against its own books, and rejects the Pix of a desenquadrado PSP with `RR04` |
| Pix Automático | Recurrences proposed by the receiver and approved by the payer (with its own cap), one charge per period debited on the due date, daily retries on `AM04`, cancellation by either party |
| Pix por aproximação | NFC payments of a BR Code from a device, with the payer's per-transaction, daily and night limits |
| Fraud risk | Every DICT lookup carries an explainable 0-100 indicator of the key owner (new key or account, MED marks, fan-in, pass-through) |
| Statements | `camt.053.001.08` statement per account and day, one entry per posting, for line-by-line reconciliation |
| Observability | Prometheus `/metrics` (no dependencies), W3C `traceparent` continued from the caller, returned on every response and sent on the webhooks of the transfer, `trace_id` in every log line |
| Webhooks | `pix.received`, `pix.settled`, `pix.rejected`, `refund.completed`; HMAC-SHA256 signatures; retries with exponential backoff and jitter; delivery log; redelivery |
| Chaos | Per-request headers and global rules: forced rejection codes, probabilistic timeouts, latency, duplicate and out-of-order webhooks |
| Time travel | `POST /admin/clock/advance` moves the simulator clock; due settlements and retries run immediately, charges and claims expire |
| Operations | Embedded dashboard at `/`, OpenAPI 3.1 at `/openapi.json`, `/healthz`, JSON logs with request IDs, problem+json errors, `Idempotency-Key` |

## How it fits

```mermaid
sequenceDiagram
    autonumber
    participant C as Your service
    participant S as pix-psp-simulator
    participant W as Your webhook endpoint

    C->>S: GET /dict/keys/{key} (X-Payer-Id)
    S-->>C: 200 owner name, masked tax id, ISPB, account
    C->>S: POST /pix/transfers (Idempotency-Key, X-Sim-* optional)
    S-->>C: 202 status ACSP, end_to_end_id
    Note over S: settlement latency (default 500ms)<br/>chaos rules evaluated
    alt settled
        S->>W: POST pix.settled (payer) / pix.received (payee)<br/>X-Signature: t=...,v1=...
    else rejected
        S->>W: POST pix.rejected (reason AM04, AC03, AB03...)
    end
    W-->>S: 2xx (anything else is retried with backoff + jitter)
    C->>S: GET /pix/transfers/{e2eid}
    S-->>C: 200 status ACSC or RJCT
```

## Quickstart

Run it from source (Go 1.22+):

```sh
go run ./cmd/pixsim -scenarios scenarios/basic.json -log-format text
```

or with Docker Compose, which also starts an example consumer that verifies
signatures and drops duplicates:

```sh
docker compose up --build
open http://localhost:8080          # dashboard
```

The examples below assume the simulator on `localhost:8080` and the example
receiver on `localhost:9000` with secret `whsec_demo`
(`go run ./examples/webhook-receiver -secret whsec_demo` if you are not using
Compose; inside Compose use `http://receiver:9000/pix` as the webhook URL).

**1. Create accounts** (or rely on `scenarios/basic.json`, which already has
`alice`, `bob`, `padaria` and a closed account):

```sh
curl -s localhost:8080/accounts -H 'Content-Type: application/json' -d '{
  "id": "carol", "owner_name": "Carol Lima", "document": "111.444.777-35",
  "initial_balance_cents": 20000
}'
```

**2. Register a key** and look it up the way a payer's app would:

```sh
curl -s localhost:8080/dict/keys -H 'Content-Type: application/json' \
  -d '{"account_id": "carol", "key": "Carol@Example.com"}'

curl -s localhost:8080/dict/keys/carol@example.com -H 'X-Payer-Id: alice'
# { "key": "carol@example.com", "key_type": "EMAIL", "owner_name": "Carol Lima",
#   "owner_tax_id_masked": "***.444.777-**", "owner_type": "NATURAL_PERSON", ... }
```

**3. Register a webhook** for the payee:

```sh
curl -s -X PUT localhost:8080/accounts/carol/webhook -H 'Content-Type: application/json' \
  -d '{"url": "http://localhost:9000/pix", "secret": "whsec_demo"}'
```

**4. Transfer** and watch it settle:

```sh
curl -s localhost:8080/pix/transfers -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-1001' \
  -d '{"from_account_id": "alice", "key": "carol@example.com", "amount_cents": 2500}'
# 202 { "end_to_end_id": "E99999999202611211559...", "status": "ACSP", ... }

curl -s localhost:8080/pix/transfers/E99999999202611211559...   # "status": "ACSC"
```

**5. Receive the webhook.** The receiver logs a verified `pix.received`
event. The delivery log shows every attempt:

```sh
curl -s 'localhost:8080/webhooks/deliveries?account_id=carol'
```

**6. Inject failures:**

```sh
# Rejected for insufficient funds; pix.rejected goes to the payer's webhook
# (if alice has one registered) twice, with the same event ID
curl -s localhost:8080/pix/transfers -H 'Content-Type: application/json' \
  -H 'X-Sim-Fail: AM04' -H 'X-Sim-Webhook-Duplicates: 1' \
  -d '{"from_account_id": "alice", "key": "carol@example.com", "amount_cents": 100}'

# SPI timeout after 5 seconds, 30% of the time
curl -s localhost:8080/pix/transfers -H 'Content-Type: application/json' \
  -H 'X-Sim-Fail: AB03' -H 'X-Sim-Fail-Probability: 0.3' -H 'X-Sim-Latency: 5s' \
  -d '{"from_account_id": "alice", "key": "carol@example.com", "amount_cents": 100}'
```

**7. Charges and time travel:**

```sh
curl -s localhost:8080/cob -H 'Content-Type: application/json' -d '{
  "type": "dynamic", "receiver_account_id": "padaria",
  "amount_cents": 1590, "expires_in_seconds": 600
}'
# -> "brcode": "000201010212266700...", "location": "http://localhost:8080/qr/v2/..."

curl -s localhost:8080/pix/qr-payments -H 'Content-Type: application/json' \
  -d '{"from_account_id": "alice", "brcode": "<brcode from above>"}'

# Create another charge, leave it unpaid, and jump past its expiration:
curl -s localhost:8080/admin/clock/advance -H 'Content-Type: application/json' \
  -d '{"duration": "15m"}'
curl -s localhost:8080/cob/<txid>   # "status": "REMOVIDA_PELO_PSP", "expired": true
```

Malformed codes are rejected with brpay's lint report:

```json
{
  "type": "urn:pixsim:problem:brcode_lint_failed",
  "status": 422,
  "code": "brcode_lint_failed",
  "issues": [
    { "severity": "error", "field": "63", "message": "CRC is \"ABCD\", expected 5F03 (...)" },
    { "severity": "error", "field": "54", "message": "amount \"100,00\": uses a comma; the decimal separator must be a dot" }
  ]
}
```

**8. Refund and MED:**

```sh
E2E=E99999999202611211559...
curl -s localhost:8080/pix/transfers/$E2E/refunds -H 'Content-Type: application/json' \
  -d '{"amount_cents": 500, "reason": "MD06"}'

curl -s localhost:8080/med/infractions -H 'Content-Type: application/json' \
  -d "{\"end_to_end_id\": \"$E2E\", \"details\": \"social engineering\"}"
curl -s -X POST localhost:8080/med/infractions/inf_.../block
curl -s -X POST localhost:8080/med/infractions/inf_.../analyze
curl -s -X POST localhost:8080/med/infractions/inf_.../accept   # held funds go back to the payer (FR01)
```

**9. Report to the regulator.** Register the PSP with the simulated STA,
mapping a Cosif account to the simulator accounts that hold its money, then
fetch the 4111 the simulator expects for yesterday and send it:

```sh
curl -s -X PUT localhost:8080/sta/participants/99999999 -H 'Content-Type: application/json' -d '{
  "ispb": "99999999", "reconcile": [{ "conta": "1120000006", "account_ids": ["alice", "bob", "padaria"] }],
  "reconcile_volume": true
}'
curl -s localhost:8080/sta/participants/99999999/drafts/4111 > 4111.xml
curl -s localhost:8080/sta/documents -H 'Content-Type: application/xml' --data-binary @4111.xml
# { "protocolo": "STA20261123000001", "status": "ACEITO", "prazo": "2026-11-26", "fora_do_prazo": false, ... }
curl -s localhost:8080/sta/participants/99999999   # missing days and months, desenquadrado or not
```

The full API, including request and response schemas and the webhook payload,
is in [`/openapi.json`](internal/api/openapi.json).

## Regulator side: STA and CADOCs

`POST /sta/documents` plays the Banco Central's Sistema de Transmissão de
Arquivos for two accounting documents. Every file gets a protocol, `ACEITO`
or `REJEITADO`, with the errors listed the way a processing report would:

| Rule | 4111 (daily) | 4010 (monthly) |
|------|--------------|----------------|
| Header | `codigoDocumento`, `cnpj` (8-character CNPJ base), `dataBase` (`AAAA-MM-DD` / `AAAA-MM`), `tipoRemessa` `I` or `S` | same |
| Lines | `conta` with 10 digits, unique; `saldoDia` with up to 18 digits and 2 decimals after a dot | `saldo`, same format |
| Data-base | A business day that is over; weekends and national holidays are refused (their volume goes into the next business day) | A closed month |
| Replacement | `I` when nothing was accepted for the date, `S` to replace an accepted file | same |
| Sequence | The previous business day must be accepted first | |
| Deadline | 3rd business day after the data-base; later files are accepted with `fora_do_prazo` | Last business day of the following month (a simulator assumption) |
| Reconciliation | Mapped Cosif accounts must equal the simulator balances at the end of the day (journal replay); the 3.0.9.70 accounts must equal the Pix volume settled for the ISPB in the month | |
| Cross-check | On the last business day of a month, balances must agree with the accepted 4010 | Balances must agree with the 4111 of the month's last business day |

A participant that owes a 4010 past its deadline is **desenquadrado**; with
`"enforce": true` its outgoing Pix end `RJCT` with `RR04` until the 4010 is
accepted. The calendar (national holidays and the days the financial system
does not operate, in Brasília time) is at `GET /admin/calendar`. Header
fields and rules follow the published 4111 filling instructions; the XML
element names (`<documento>`, `<contas>`, `<conta>`) are the simulator's, so
validate a real file against the Banco Central's XSD.

## MED 2.0

With MED 2.0 (the default), blocking an infraction blocks what the payee
still has and follows the rest, first in first out, into the transfers the
payee made after receiving the money, breadth first, up to
`-med-max-accounts` accounts (5). The infraction's `trace` lists every hop
(depth, account, ISPB, the transfer that brought the money there, traced and
held amounts). Accepting returns everything that was blocked to the victim
in one `FR01` devolução; rejecting or cancelling releases every hop. A block
that nobody analyses within `-med-block-window` (72h) lapses and the
infraction ends `EXPIRED`. Funds traced back to the victim are not blocked.
Accepted infractions become fraud marks in the DICT `fraud_risk` of every
account they reached.

## Pix Automático and Pix por aproximação

```sh
curl -s localhost:8080/pix/automatico/recurrences -H 'Content-Type: application/json' -d '{
  "receiver_account_id": "padaria", "payer_account_id": "alice", "frequency": "MONTHLY",
  "start_date": "2026-12-01", "max_amount_cents": 9000, "description": "assinatura"
}'
curl -s -X POST localhost:8080/pix/automatico/recurrences/rec_.../approve -d '{"payer_max_amount_cents": 5000}'
curl -s localhost:8080/pix/automatico/recurrences/rec_.../charges -d '{"amount_cents": 4990, "due_date": "2026-12-05"}'
# debited at 06:00 on the due date (try POST /admin/clock/advance), retried daily on AM04
```

`POST /pix/proximity-payments` takes the same body as `/pix/qr-payments`
plus a `device_id` and enforces the payer's limits from
`PUT /accounts/{id}/limits` (`proximity_per_transaction_cents`,
`proximity_daily_cents`, `night_per_transaction_cents` from 20h to 6h).
There is no fixed cap on the amount. The notice window, retry count, debit
hour and night period are simulator settings, not a statement of the rules.

## Observability

- `GET /metrics` serves Prometheus text format: `pixsim_http_requests_total`
  and `pixsim_http_request_duration_seconds` by route,
  `pixsim_transfers_total{status,reason,initiation}`,
  `pixsim_settlement_latency_seconds`, `pixsim_dict_lookups_total`,
  `pixsim_webhook_attempts_total`, `pixsim_med_*`, `pixsim_sta_submissions_total`,
  `pixsim_automatico_*`, `pixsim_proximity_payments_total`, and gauges for
  system balances, in-flight transfers, open infractions and pending
  deliveries. `pixsim_ledger_total_cents` other than 0 is a bug.
- A request's `traceparent` is continued (or a trace is started), echoed in
  the response, stored with the transfer and sent on its webhooks, so one
  Pix is one trace from the PSP's API call to its webhook consumer. Log
  lines carry `trace_id` and `span_id`.
- `GET /iso/camt.053?account_id=&date=` returns the day's statement of an
  account for reconciliation.

## Verifying webhooks

Every delivery carries:

| Header | Meaning |
|--------|---------|
| `X-Signature` | `t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<raw body>")>` |
| `X-Event-Id` | Stable across retries and chaos duplicates: deduplicate on it |
| `X-Event-Type` | `pix.received`, `pix.settled`, `pix.rejected`, `refund.completed`, `automatico.recurrence.updated`, `automatico.charge.updated` |
| `traceparent` | W3C trace context of the request that created the transfer (when it had one) |
| `X-Delivery-Id`, `X-Delivery-Attempt` | Which copy and which try this is |

Import the helper instead of re-implementing it:

```go
import "github.com/victorcondackdev/pix-psp-simulator/webhook"

body, _ := io.ReadAll(r.Body)
if err := webhook.Verify(secret, r.Header.Get(webhook.SignatureHeader), body, 5*time.Minute); err != nil {
    http.Error(w, "invalid signature", http.StatusUnauthorized)
    return
}
var ev webhook.Event
_ = json.Unmarshal(body, &ev)
```

`Verify` accepts several `v1` values (secret rotation), compares in constant
time and rejects timestamps outside the tolerance. Signatures use wall-clock
time even after the simulator clock has been advanced, so consumers verifying
with their own clock keep working. Failed deliveries (non-2xx, timeouts,
connection errors) are attempted up to 6 times, waiting `1s, 2s, 4s, ...`
(capped at 5 minutes, each with equal jitter) between attempts.

## Chaos headers

Accepted by `POST /pix/transfers`, `POST /pix/qr-payments` and
`POST /pix/transfers/{e2eid}/refunds`. Headers override global rules.

| Header | Example | Effect |
|--------|---------|--------|
| `X-Sim-Fail` | `AM04` | Reject with this reason code after the settlement latency |
| `X-Sim-Fail-Probability` | `0.25` | Apply `X-Sim-Fail` only with this probability |
| `X-Sim-Timeout` | `true` | Reject with `AB03` (settlement timeout) |
| `X-Sim-Latency` | `2s` | Override the settlement latency for this request |
| `X-Sim-Webhook-Duplicates` | `2` | Send each resulting webhook N extra times, same event ID |
| `X-Sim-Webhook-Reorder` | `3s` | Delay each delivery by a random amount up to this, so events can arrive out of order |

Supported reason codes, plus those the simulator produces on its own:

| Code | Meaning | Produced automatically when |
|------|---------|-----------------------------|
| `AC03` | Invalid creditor account number | Manual transfer to account details that do not exist |
| `AC04` | Closed account number | Payee account is `CLOSED` |
| `AC06` | Blocked account | Payee account is `BLOCKED` |
| `AM02` | Amount not allowed | Amount above `-max-transfer-cents` |
| `AM04` | Insufficient funds | Payer balance too low (transfers and refunds) |
| `AB03` | Settlement aborted due to timeout | Chaos only |
| `BE01` | Inconsistent with end customer | Chaos only |
| `DS04` | Order rejected | Chaos only |
| `RR04` | Regulatory reason | The payer's PSP is desenquadrado at the STA and its participant has `enforce` on |

## Scenario files

Scenarios seed accounts, keys, webhooks, chaos rules, the DICT rate limit,
account limits (`"limits": {"alice": {"night_per_transaction_cents": 100000}}`)
and STA participants (`"sta_participants": [...]`, the body of
`PUT /sta/participants/{cnpj}` plus `cnpj`).
Load them at startup (`-scenarios 'scenarios/*.json'`, applied in file-name
order) or at runtime with `POST /admin/scenarios`. Unknown fields are
rejected, so typos fail loudly.

```json
{
  "name": "checkout",
  "description": "Merchant with noisy webhooks and a flaky SPI",
  "accounts": [
    { "id": "alice", "owner_name": "Alice Santos", "document": "529.982.247-25",
      "initial_balance_cents": 500000 },
    { "id": "shop", "owner_name": "Loja Exemplo Ltda", "document": "12.ABC.345/01DE-35",
      "ispb": "12345678", "branch": "0042", "number": "778899", "type": "CACC",
      "webhook": { "url": "http://localhost:9000/pix", "secret": "whsec_demo" } },
    { "id": "gone", "owner_name": "Old Account", "document": "111.444.777-35", "status": "CLOSED" }
  ],
  "keys": [
    { "account_id": "shop", "key": "12ABC34501DE35" },
    { "account_id": "shop", "evp": true }
  ],
  "chaos_rules": [
    { "id": "flaky-spi", "probability": 0.05, "match": { "operation": "qr_payment" },
      "timeout": true, "latency": "3s" },
    { "id": "big-tickets", "match": { "operation": "transfer", "min_amount_cents": 1000000 },
      "fail": "AM02" },
    { "id": "noisy-webhooks", "match": { "to_account": "shop" },
      "duplicate_webhooks": 1, "reorder_webhooks": "2s" }
  ],
  "dict_rate_limit": { "capacity": 100, "refill_per_minute": 20, "not_found_cost": 3 }
}
```

Rule fields: `match` (`operation`: `transfer`, `qr_payment` or `refund`;
`from_account`; `to_account`; `min_amount_cents`; `max_amount_cents`; all
optional), `probability` (0 to 1, default 1), and any of `fail`, `timeout`,
`latency`, `duplicate_webhooks`, `reorder_webhooks`. Durations are Go
duration strings or milliseconds. Rules are evaluated in order and later
matches override earlier ones; manage them at runtime under
`/admin/chaos/rules`.

## Configuration

Every flag has a `PIXSIM_*` environment variable (`-settlement-latency` is
`PIXSIM_SETTLEMENT_LATENCY`); flags win.

| Flag | Default | |
|------|---------|---|
| `-addr` | `:8080` | Listen address |
| `-ispb` | `99999999` | Simulator ISPB, used in EndToEndIds and new accounts |
| `-base-url` | `http://localhost:<port>` | Public URL put in dynamic QR codes |
| `-settlement-latency` | `500ms` | Time from `ACSP` to `ACSC`/`RJCT` |
| `-charge-expiry` | `1h` | Default expiration of dynamic charges |
| `-max-transfer-cents` | `0` | Larger transfers are rejected with `AM02` (0 disables) |
| `-scenarios` | | Comma-separated files or globs |
| `-seed` | `0` | Seed for chaos probabilities and jitter; set it for reproducible runs |
| `-med-block-window` | `72h` | How long an unanalysed MED block lasts |
| `-med-max-accounts` | `5` | Accounts MED 2.0 tracing may visit, payee included |
| `-webhook-max-attempts` | `6` | Attempts per delivery |
| `-webhook-base-backoff` | `1s` | First retry delay; doubles each attempt, capped at 5m |
| `-webhook-timeout` | `5s` | Per-attempt timeout |
| `-log-level`, `-log-format` | `info`, `json` | slog level and `json`/`text` |
| `-healthcheck` | | Probe `/healthz` and exit; used by the image's `HEALTHCHECK` |

## Design notes

- **Money is conserved by construction.** Every movement is a single posting
  between two accounts. Funds of in-flight transfers sit in a settlement
  account, MED blocks in a hold account, and seeded balances come from a
  funding account that is the only one allowed to go negative, so the sum of
  all balances is always zero. Tests hammer the ledger with concurrent
  transfers under `-race` and assert no negative balance and exact
  conservation.
- **State machines are data.** DICT claims and MED infractions are declared as
  transition tables (`internal/fsm`); invalid transitions return `409` with
  the allowed events.
- **Time is injected.** Everything reads a `clock.Clock`. Tests use a fake
  clock that runs timers synchronously, so retry schedules and expirations
  are asserted to the millisecond without sleeping. The server uses a virtual
  clock that can only move forward.
- **Failures are first-class.** Business rejections never fail the HTTP call:
  the transfer is accepted and ends `RJCT` with a reason and a webhook, the
  way the SPI behaves.

Layout:

```
cmd/pixsim            binary: flags, env, graceful shutdown
webhook/              public: Sign, Verify, Event (import this in consumers)
internal/api          HTTP handlers, problem+json, middleware, OpenAPI, dashboard
internal/sim          accounts, transfers, charges, refunds, MED 2.0, Pix Automático,
                      aproximação, fraud risk, statements, scenarios, telemetry
internal/sta          simulated STA: CADOC 4111 and 4010
internal/bizday       business-day calendar (Brasília)
internal/metrics      Prometheus exposition, no dependencies
internal/trace        W3C trace context
internal/dict         DICT keys, claims, rate limiting
internal/ledger       double-entry ledger
internal/notify       webhook delivery with retries
internal/chaos        rules and header overrides
internal/clock        real, fake and virtual clocks
internal/fsm          transition tables
internal/iso          ISO 20022 status and reason codes, camt.053
examples/             webhook receiver that verifies and deduplicates
scenarios/            example scenario files
```

## Development

```sh
make workspace   # go.work pointing at ../brpay, for local development
make lint        # gofmt check + go vet
make cover       # go test -race with coverage, fails under 80%
make run         # all scenarios, text logs
make docker
```

## Disclaimer

This project is not affiliated with, endorsed by or connected to the Banco
Central do Brasil or any PSP. It implements simplified models of Pix, the
SPI, the DICT, devoluções, MED, Pix Automático, Pix por aproximação and the
STA for development and testing; message formats, state machines, limits,
deadlines, rate-limit policies, fraud indicators and reason-code usage are
approximations chosen to be useful in tests, not a reference implementation
of the regulations or of any participant's behaviour. The dynamic QR payload
is served as plain JSON rather than a signed JWS, CADOC files use the
simulator's own element names, the only ISO 20022 XML is the camt.053
statement, and there is no mTLS and no persistence. Do not use it in production
or with real customer data.

## License

[MIT](LICENSE) © 2026 Victor Condack
