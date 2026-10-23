## Task: update the license

Optional input: `holder`, the copyright holder to use, and `year`, the year to bring the notice up to (default: the current year, from `date +%Y`).

1. Clone the repository and find its license file (`LICENSE`, `LICENSE.md`, `COPYING`, ...). Identify the license (MIT, Apache-2.0, BSD-3-Clause, ...) and check it against what GitHub reports for the repo.
2. Update only the copyright notice: extend the year or range to `year` (e.g. `2021-2024` becomes `2021-2026`, a single `2023` becomes `2023-2026`), and replace the holder only if `holder` is given. Keep the license text itself word for word.
3. If there is no license file, do not invent one: report that, and the license GitHub reports if any.
4. Save the new file as `/workspace/out/LICENSE` and the change as a unified diff in `/workspace/out/license.diff` (`diff -u old new`), and save both as artifacts.

Reply with a short report:
- `changed`: whether the file needed a change,
- `license`: the SPDX identifier of the license, or "none",
- `summary`: what you changed, e.g. "Copyright 2021-2024 Acme → 2021-2026 Acme".
