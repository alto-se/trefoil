import os
import subprocess


def Query(args):
    # The URL stays in the environment rather than on the command line.
    options = "-c statement_timeout=60000"
    if not getattr(args, "write", False):
        options += " -c default_transaction_read_only=on"
    env = dict(os.environ, PGOPTIONS=options, PGCONNECT_TIMEOUT="10")
    result = subprocess.run(
        ["psql", "-X", "-v", "ON_ERROR_STOP=1", "-d", env["POSTGRES_URL"], "-f", "-"],
        input=args.sql,
        env=env,
        capture_output=True,
        text=True,
        timeout=120,
    )
    return f"exit code {result.returncode}\n{result.stdout}{result.stderr}"
