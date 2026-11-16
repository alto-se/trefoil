# Needs a GitHub token that can push and open pull requests:
#   trefoil secrets set --name=github_token
# Point input.json at a repository you own before running.
trefoil config sync config.yml
trefoil agents build --agent=feature
trefoil agents run --agent=feature --input=file://input.json
