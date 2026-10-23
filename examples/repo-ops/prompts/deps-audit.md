## Task: audit dependencies

1. Clone the repository and find its dependency manifests and lock files (`go.mod`, `package.json`, `package-lock.json`, `pyproject.toml`, `requirements*.txt`, `Cargo.toml`, `Dockerfile` base images, GitHub Actions `uses:` lines).
2. For each direct dependency, note the version in use and look up the latest one with the ecosystem's own tool where installed (`go list -m -u all`, `npm outdated`, `pip index versions`). Use `npm audit` or `govulncheck` (`go run golang.org/x/vuln/cmd/govulncheck@latest ./...`) for known vulnerabilities when they apply.
3. Flag:
   - **security**: dependencies with known vulnerabilities,
   - **major**: a new major version is out,
   - **stale**: no release in over two years, or archived upstream,
   - **unpinned**: Actions pinned to a branch, or Docker images without a tag.
4. Write a Markdown report to `/workspace/out/deps-audit.md`: a summary line with counts per flag, then a table per ecosystem (name, current, latest, flags, note). Save it as an artifact.

Reply with the security findings and the three upgrades you would do first, with a reason for each.
