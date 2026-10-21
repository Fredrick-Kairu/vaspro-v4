# 0005. Error catalogue format

**Date:** 2026-10-21 · **Status:** accepted

## Context

Every service, every endpoint, needs one error body shape and one place
that lists every code it can return — system design §8: *"Each code
family has one home in the request path"*, with every synchronous error
returned as the same JSON shape, `type`/`title`/`status`/`code`/`detail`/
`retryable`/`request_id`, and D-08 fixing the code families themselves:
`AUTH`, `KYC`, `VAL`, `BAL`, `CMP`, `RTE`, `DLR`, `RATE`, `SYS`, `LIM`,
`PAY`. Task 0.5 already built this; this ADR records the two format
decisions underneath it that weren't otherwise written down: why RFC
9457 for the wire format, and why a single YAML catalogue file as the
source of truth for every code rather than codes scattered across each
service's own error definitions.

**Wire format.** The alternative to RFC 9457 (`application/problem+json`)
is a bespoke error envelope, which every client integrating with VASPRO
would have to learn from VASPRO's own docs instead of from a standard
every HTTP tooling ecosystem already understands. RFC 9457 is also
already a precise fit for what system design §8 wants: `type` as a
stable, dereferenceable link to the code's own docs page (*"every code
has a page with its cause, retry advice and fix, linked from the `type`
field"*), `status` as the real HTTP status, `title` as a stable string,
and extension members (`code`, `detail`, `retryable`, `request_id`) for
everything the standard doesn't name but VASPRO needs.

**Catalogue as data, not code.** The alternative to one
`docs/errors/catalogue.yaml` is each service defining its own error
constants directly in Go. That scales badly against two things this
project already cares about: `make check`'s quality gate (§8.3) —
*"Every error code that code can return is listed in
docs/errors/catalogue.yaml. A test fails on unknown codes."* — needs one
file to check against, and the docs site system design §8 wants (one
page per code, cause/fix/retry advice) needs one file to render from.
Scattered Go constants give neither for free.

## Decision

**`docs/errors/catalogue.yaml`** is the source of truth for every error
code: `code` (`FAMILY-NNNN`), `status`, `title`, `cause`, `fix`,
`retryable`, `billable` — the last specifically because VASPRO sometimes
charges for an attempt that still errors (DLR reason codes report a
failure through an event, after the original request already returned
success and may have been billed; `billable` records that per code,
not as a side comment). **`tools/errcodes`** (task 0.5) generates
`internal/platform/problem/codes_gen.go` from it: typed `Code` constants
and a lookup table, so a service writes `problem.New(problem.VAL4000,
"...")`, not a bare string that could typo past review.
**`internal/platform/problem.Respond`** writes the RFC 9457 body and
sets `request_id` from context; **`internal/platform/httpx.Recover`**
turns a panic into `SYS-9000` with no stack trace in the body (system
design §8: *"Stack traces and internal messages never leave the
service"*).

The catalogue is deliberately partial today — one representative code
per family, exactly enough for Phase 0's own tests (the file's own header
comment says so). Each service adds the rest of its own codes to this
same file before its own phase starts, the same way OpenAPI specs are
written just in time (ROADMAP §10, item A5) — nothing about the format
changes as codes are added, only the row count.

## Consequences

- Every error response in the system, from every service, is the same
  shape — a client (or VASPRO's own e2e suite, §8.3: *"Every public
  endpoint has an OpenAPI example that runs green"*) parses one format,
  not fourteen services' own conventions.
- Adding a new error code is a YAML row plus `make gen` — never a raw
  string literal in a handler. `tools/errcodes`'s generated constants
  mean an unknown code is a compile error if referenced via
  `problem.<CODE>`, and a `make check` test failure if a response
  somehow carries a code absent from the catalogue (§8.3's gate).
- `billable` has to be judged honestly per code as each service adds its
  own — getting it wrong doesn't break a test today (nothing yet reads
  it), but it's exactly the kind of fact that's cheap to record
  correctly now and expensive to reconstruct later once billing logic
  depends on it.
- DLR reason codes (`DLR-7001` and siblings) reuse the exact same
  catalogue and `Code` type as synchronous HTTP errors, even though
  they're delivered through an event, not a response body — one format,
  two delivery paths, not two formats.
