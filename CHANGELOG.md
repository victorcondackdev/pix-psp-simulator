# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Simulated STA (`/sta/...`) receiving CADOC 4111 (Saldos Contábeis Diários)
  and 4010 (Balancete): layout, business-day data-base, I/S replacement,
  sequence and deadline rules; reconciliation of mapped Cosif accounts and of
  the 3.0.9.70 payment volume against the simulator books; 4111 × 4010
  cross-check; desenquadramento with optional `RR04` rejection of the PSP's
  Pix; expected 4111 draft; business-day calendar at `/admin/calendar`.
- MED 2.0: funds tracing through up to `-med-max-accounts` accounts with a
  per-hop `trace`, release and return per hop, and a cautionary block that
  lapses after `-med-block-window` (new `EXPIRED` status, `expire` event).
- Pix Automático: recurrences, payer approval with its own cap, one charge per
  period with notice window, debit on the due date with daily retries,
  cancellation; `automatico.*` webhooks; initiation `AUTO`.
- Pix por aproximação (`POST /pix/proximity-payments`) with per-account
  proximity, daily and night limits (`/accounts/{id}/limits`).
- `fraud_risk` on DICT lookups: explainable score from the simulator history.
- `camt.053.001.08` statements (`GET /iso/camt.053`).
- Prometheus `/metrics`; W3C `traceparent` continued, echoed, stored with
  transfers and sent on their webhooks; `trace_id`/`span_id` in logs.
- Scenario files accept `limits` and `sta_participants`.

### Changed

- MED infractions default to `med_version` `2.0`. When the payee still holds
  the funds the outcome is the same as before; pass `"med_version": "1.0"` for
  the payee-only block.

## [0.1.0] - 2026-11-21

First public release.

### Added

- DICT mock: register, delete and look up CPF, CNPJ (numeric and alphanumeric),
  e-mail, phone and EVP keys, normalized with brpay; masked owner documents;
  per-account key limits; ownership and portability claims with a resolution
  period; per-payer token-bucket rate limiting with a not-found penalty.
- Charges (`/cob`): static and dynamic BR Codes generated with brpay, dynamic
  payload locations served at `/qr/v2/{id}`, expiration, removal.
- Payments by key, by account details, by account ID and by BR Code
  ("copia e cola"); BR Codes are linted and lint errors are returned as
  problem details.
- Asynchronous settlement with configurable latency, ACSC/RJCT outcomes and
  ISO 20022 reason codes (AB03, AC03, AC04, AC06, AM02, AM04, BE01, DS04,
  RR04), on an in-memory double-entry ledger.
- Devoluções (full and partial, BE08/MD06/SL02) and a simplified MED flow
  (OPEN, BLOCKED, ANALYSIS, REFUNDED, CLOSED, CANCELLED) with cautionary block
  of funds.
- Signed webhooks (`X-Signature: t=...,v1=...`, HMAC-SHA256) with retries,
  exponential backoff with jitter, delivery log and redelivery; public
  `webhook` package with `Sign`, `Verify` and `VerifyAt`.
- Failure injection by request headers (`X-Sim-*`) and global rules: forced
  rejections, probabilistic timeouts, latency, duplicate and out-of-order
  webhook deliveries.
- JSON scenario files loaded at startup or through `POST /admin/scenarios`.
- Virtual clock with `POST /admin/clock/advance`.
- `Idempotency-Key` support on `POST /pix/transfers`.
- Embedded dashboard, embedded OpenAPI 3.1 document, `/healthz`, JSON logs
  with request IDs.
- `pixsim` binary configurable by flags and `PIXSIM_*` variables, distroless
  Docker image, docker-compose example with a verifying webhook consumer.

[0.1.0]: https://github.com/victorcondackdev/pix-psp-simulator/releases/tag/v0.1.0
