# Needs a GitHub token on the server, e.g. in .env:
#   export TREFOIL_SECRET_GITHUB_TOKEN=github_pat_...
trefoil config sync config.yml
trefoil agents build --agent=github
trefoil agents run --agent=github --input=file://input.json
