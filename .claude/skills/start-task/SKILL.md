---
name: start-task
description: Start the current build task from docs/STATUS.md with a plan
disable-model-invocation: true
---
1. Read docs/STATUS.md. Identify the current phase and task. If $ARGUMENTS names a task, use that instead.
2. Read that phase's section in docs/ROADMAP.md, then only the design sections it lists under "Read first" that relate to this task.
3. Check whether any open decision (docs/ROADMAP.md §4.3) or open question blocks the task. If one does and has no default, stop and say so.
4. Produce a plan with:
   - contracts to add or change (OpenAPI, migrations, sqlc, proto, events, error codes)
   - tests to write first, as a list of behaviours
   - files to create or change
   - risks and questions
5. Wait for approval before changing any file.
