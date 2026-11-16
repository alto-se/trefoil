from pathlib import Path
import subprocess

def RunNode(args):
    path = Path(args.path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(args.code)
    result = subprocess.run(
        ["node", str(path)],
        cwd=path.parent,
        capture_output=True,
        text=True,
        timeout=getattr(args, "timeout", 60),
    )
    return f"wrote {path}\nexit code {result.returncode}\n{result.stdout}{result.stderr}"
