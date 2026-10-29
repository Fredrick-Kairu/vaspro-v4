# 0001. Event bus: NATS JetStream

**Date:** 2026-10-21 · **Status:** accepted

## Context

Every service change that another part of the system needs to know about
leaves through a transactional outbox and arrives through an event bus
(data model §1, D-06; system design §1). Two candidates were on the
table from the start (ROADMAP §4.3, OD-01: *"Event bus: NATS JetStream or
Kafka"*), with a documented default — *"NATS JetStream behind the
`platform/bus` interface"* — and an explicit **needed-by** of Phase 0,
since `platform/outbox`'s relay (task 0.8) and the first real consumer
need a concrete bus to publish through and a durable-consumer shape to
retry against, not just an interface.

Kafka is the obvious alternative: proven at far higher volumes, a larger
operational ecosystem. Against that: VASPRO's Phase 0–2 volumes (Kenya
parity first) don't need Kafka's throughput ceiling, Kafka needs
ZooKeeper or KRaft plus its own operational surface the team doesn't yet
have, and NATS JetStream already gives durable streams, pull consumers,
redelivery and dead-lettering — everything D-06 and `.claude/rules/tests.md`
ask for — with one binary and a much smaller operational footprint
locally and in CI (`deploy/compose/docker-compose.yml` already runs it,
task 0.2).

## Decision

**NATS JetStream**, reached through `platform/bus.Bus` and nothing else
in the codebase. `Bus.Publish` satisfies `platform/outbox.Publisher`
directly (closing the interface task 0.8 defined specifically so the
outbox wouldn't have to know about NATS); `Bus.Consume` runs a durable
pull consumer with a configurable `MaxDeliver` and a hand-rolled
dead-letter subject — JetStream has no server-side "move failed messages
to this subject" configuration at the client API level, so `Consume`
tracks delivery count itself and publishes to the dead-letter subject
before `Term`-ing the final attempt (`internal/platform/bus/bus.go`).

`platform/testkit.StartNATS`/`StartNATSURL` (task 0.12) give every other
package's integration tests a real JetStream container in two lines
instead of hand-rolling the container-start boilerplate, and
`platform/bus`'s own integration tests prove a full Postgres+NATS round
trip: `outbox.Write` → `Relay.RelayOnce` → `Bus.Publish` →
`Bus.Consume` → redelivery-after-failure → dead-letter-after-`MaxDeliver`
(`bus_integration_test.go`).

If NATS JetStream turns out not to scale for some later phase's volume
(the OD-01 escape hatch this ADR exists to use if needed), that's a
`platform/bus` rewrite behind the same `Publisher`/`Consume` shapes
every caller already depends on — not a rewrite of `outbox`, `inbox`, or
any service.

## Consequences

- Every service's event publishing and consumption goes through
  `platform/bus`, never a direct `nats.go` import — the one place that
  would have to change if this decision is revisited.
- Local dev and CI both need a NATS container (`deploy/compose/`,
  `platform/testkit`); no separate ZooKeeper/KRaft cluster to run or
  reason about.
- Dead-lettering, retry counts and stream/consumer naming are this
  codebase's own code (`bus.ConsumeConfig`), not a broker feature — any
  change to that behavior is a Go change, reviewable and testable like
  any other, but also work `platform/bus` owns rather than configuration
  NATS gives for free.
- Throughput headroom is NATS JetStream's, not Kafka's. Revisit this ADR
  (superseding it, not editing it) if a phase's load test shows that
  ceiling approaching.
