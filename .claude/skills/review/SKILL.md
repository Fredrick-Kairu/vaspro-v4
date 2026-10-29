---
name: review
description: Fresh-eyes review of a change against the VASPRO v4 design and checklists
disable-model-invocation: true
---
Review the changes in $ARGUMENTS (a branch, commit range or files; default: the diff against main). You did not write this code; review it as a strict senior engineer.

Check, and report each finding with file, line, severity and a concrete fix:
1. Matches the design: data model names and constraints, system design flows, ADRs.
2. Money: minor units, no floats, balanced postings, idempotency, holds released on every failure path.
3. Security: authz on every endpoint, tenant isolation (RLS and tenant_id filters), no secrets or personal data in logs or errors, input validation.
4. Contracts: OpenAPI, events and error codes updated and consistent with the code; SMS engine and rail payloads match the golden files.
5. Tests: every rule and failure path covered; no skipped or weakened tests; no sleeps; deterministic time.
6. Boundaries: no cross-service imports or schema access; domain/ free of I/O.
7. Operability: logs, metrics, alerts; migrations reversible or marked.
End with a verdict: approve, or request changes with the must-fix list first.
