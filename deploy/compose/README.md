# Local stack

Infrastructure for running services on your machine (`make run`). The containerised dev environment for testers and the frontend team is `docker-compose.dev.yml`: see [docs/runbooks/dev-environment.md](../../docs/runbooks/dev-environment.md).

```
cp deploy/compose/env.example deploy/compose/.env   # once
make up      # start everything, wait for health
make down    # stop, keep data
make reset   # wipe data, start clean
```

`make up` doesn't return until every container reports healthy, so a green `make up` means the stack is actually usable, not just started.

| Service | Reach it at | Notes |
|---|---|---|
| PostgreSQL 16 | `localhost:5433` (remapped — 5432 is commonly taken by other local projects) | one instance; each service gets its own schema via its own migrations |
| Redis 7 | `localhost:6379` | sessions, rate limits, idempotency keys, USSD sessions |
| NATS (JetStream) | `localhost:4222`, monitoring on `:8222` | event bus |
| RabbitMQ | `localhost:5672`, management UI on `:15672` | SMS engine hand-off |
| ClickHouse | HTTP `localhost:8123`, native protocol `localhost:9004` (remapped to avoid the object storage API port) | per-message events, analytics |
| Object storage (SeaweedFS, S3 API) | API `localhost:9000`, master/cluster status `localhost:9001` | KYC and application documents. Stands in for MinIO, which was pulled from Docker Hub (see below) — same S3 API, so `platform/...` object-storage code targets either unchanged |
| Mailpit | SMTP `localhost:1025`, web UI `localhost:8025` | catches every local outbound email |
| Grafana / `otel-lgtm` | `localhost:3000` (Grafana), OTLP in on `:4317` (gRPC) / `:4318` (HTTP) | traces, metrics, logs in one box |

Credentials come from `.env` (see `env.example`); they're local-dev placeholders, not secrets, and `.env` itself is gitignored.

**Why SeaweedFS instead of MinIO.** ROADMAP §3.1/§11 (task 0.2) call for MinIO as the local S3-compatible store. As of this task, `minio/minio` has been removed from Docker Hub — `docker pull` fails even after `docker login`, and Docker Hub's API confirms the repository no longer exists. SeaweedFS's S3 gateway (`chrislusf/seaweedfs`, actively published, 31M+ pulls) is a drop-in replacement at the API level. If MinIO comes back or a different provider is preferred later, swap the `object-storage` service image/command — nothing elsewhere depends on it being MinIO specifically.
