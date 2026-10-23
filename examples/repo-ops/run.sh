# Needs a GitHub token on the server:
#   trefoil secrets set --name=github_token
# The repo comes from the `repo` param; override it per run with
# {"repo": "owner/name"} in the input.
trefoil config sync config.yml
trefoil params set --name=repo "${REPO:?set REPO=owner/name}"
for agent in tests release-notes license triage deps-audit; do
  trefoil agents build --agent=$agent
done

trefoil agents run --agent=tests --input=file://inputs/tests.json
trefoil agents run --agent=release-notes --input=file://inputs/release-notes.json
trefoil agents run --agent=license --input=file://inputs/license.json
trefoil agents run --agent=triage --input=file://inputs/triage.json
trefoil agents run --agent=deps-audit --input=file://inputs/deps-audit.json
