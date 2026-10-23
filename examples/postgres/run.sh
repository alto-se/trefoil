# Needs a database URL on the server, e.g. in .env:
#   export TREFOIL_SECRET_POSTGRES_URL=postgres://user:pass@host:5432/db
# or: trefoil secrets set --name=postgres_url
trefoil config sync config.yml
trefoil agents build --agent=postgres
trefoil agents run --agent=postgres --input=file://input.json
