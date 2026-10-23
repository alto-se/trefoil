## Task: run the test suite

Optional input: `ref`, a branch or tag to test instead of the default branch.

1. Clone the repository at `ref` (if given).
2. Work out how the project runs its tests: look at the CI workflows in `.github/workflows/`, a `Makefile`, `package.json` scripts, `go.mod`, `pyproject.toml` and the README. Prefer the command CI uses. Go, Node.js and Python are installed; if the project needs another toolchain, say so instead of guessing.
3. Install dependencies and run the tests from the repository root. Save the full output to `/workspace/out/test-output.txt` and save it as an artifact.
4. If tests fail, read the failing tests and the code they cover so you can explain each failure in one sentence. Do not change the code to make them pass.

Reply with a short report:
- `passed`: whether the whole suite passed,
- `command`: the command you ran,
- `summary`: two or three sentences on what ran and how it went (counts of passed/failed/skipped tests if the runner reports them),
- `failures`: one entry per failing test, "test name: likely cause", or an empty list.
