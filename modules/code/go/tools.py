from pathlib import Path
import os
import subprocess

def RunGo(args):
    path = Path(args.path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(args.code)
    env = {**os.environ, "GOCACHE": "/tmp/go-cache", "GOPATH": "/tmp/go"}
    result = subprocess.run(
        ["go", "run", path.name],
        cwd=path.parent,
        env=env,
        capture_output=True,
        text=True,
        timeout=getattr(args, "timeout", 120),
    )
    return f"wrote {path}\nexit code {result.returncode}\n{result.stdout}{result.stderr}"
