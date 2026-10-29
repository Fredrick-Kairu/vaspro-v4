---
paths:
  - "**/*_test.go"
  - "test/**"
---
# Test rules
- Write the test before the code and show it failing for the right reason.
- Table-driven tests for rules and state machines; name each case after the behaviour.
- Use platform/testkit for PostgreSQL, NATS, Redis and RabbitMQ (testcontainers). No shared state between tests.
- Use the reserved test numbers from packs/<cc>/sandbox.yaml. Never real numbers.
- Control time with the fake clock. No sleeps to wait for async work; poll with a timeout helper.
- Golden files live in testdata/; update them only on purpose (`-update` flag) and review the diff.
- Never skip or weaken a test to get green. If a test is wrong, say why and fix the test in its own commit.
- Integration tests use the build tag `integration`; end-to-end tests use `e2e`.
