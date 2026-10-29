---
name: finish-task
description: Verify, record and wrap up the current build task
disable-model-invocation: true
---
1. Run `make check`. If anything fails, fix it and run again. Do not skip or weaken tests.
2. Go through the definition of done in docs/ROADMAP.md §8.6 for this task and state what is met and what isn't.
3. Update docs/STATUS.md: tick the task, set the next task, record blockers, and add up to five short notes for the next session.
4. Tick the task's checkbox in docs/ROADMAP.md if it is complete.
5. Propose a conventional commit message and a pull request description (what changed, tests added, design sections followed, ADRs).
