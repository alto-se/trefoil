from pathlib import Path
import subprocess

def Read(args):
    return Path(args.path).read_text()

def Write(args):
    path = Path(args.path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(args.content)
    return f"Wrote {len(args.content)} characters to {path}"

def Bash(args):
    result = subprocess.run(
        args.command,
        shell=True,
        capture_output=True,
        text=True,
        timeout=getattr(args, "timeout", 120),
    )
    return f"exit code {result.returncode}\n{result.stdout}{result.stderr}"
