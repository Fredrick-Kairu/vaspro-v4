---
name: start-phase
description: Kick off a new build phase from docs/ROADMAP.md
disable-model-invocation: true
---
1. Confirm the previous phase's exit gate in docs/ROADMAP.md is fully ticked, with evidence. If not, list what is missing and stop.
2. Read the section for phase $ARGUMENTS in docs/ROADMAP.md and the design sections under "Read first".
3. Check every "Blocked until" condition and every open decision the phase needs. Report any that are unresolved.
4. Check the phase's contracts exist (OpenAPI for its services, data model sections, event schemas). If any is missing, propose drafting it first.
5. Break the phase into tasks in order, each with its tests, and write them into docs/STATUS.md as the plan for the phase.
6. Wait for approval, then start the first task with /start-task.
