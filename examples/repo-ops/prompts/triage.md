## Task: triage open issues

Optional input: `limit`, how many of the most recently updated open issues to look at (default 10).

1. List the open issues (not pull requests) and the labels the repository already uses.
2. For each issue, read the body and the latest comments, and decide:
   - **kind**: bug, feature, question, docs or chore,
   - **priority**: P1 (broken for many users, security, data loss), P2 (real problem with a workaround) or P3 (nice to have),
   - **labels**: existing labels that fit; suggest a new label only when none do,
   - **duplicates**: other open or closed issues it repeats,
   - **next step**: one sentence, e.g. "ask for a reproduction", "ready for a PR", "close as answered".
   When an issue points at code, clone the repository and check the code before calling it a bug.
3. Write a Markdown report to `/workspace/out/triage.md`: a table with one row per issue (number, title, kind, priority, labels, next step), followed by the P1 issues and the duplicate groups. Save it as an artifact.

Reply with the P1 issues and the duplicates, and the report's artifact name.
