from pathlib import Path
import subprocess

VENV = Path("/opt/venv/bin")

def RunPython(args):
    out = ""
    packages = getattr(args, "packages", None) or []
    if packages:
        pip = subprocess.run(
            [str(VENV / "pip"), "install", "--disable-pip-version-check", "-q", *packages],
            capture_output=True,
            text=True,
            timeout=300,
        )
        if pip.returncode != 0:
            return f"pip install failed\nexit code {pip.returncode}\n{pip.stdout}{pip.stderr}"
        out = f"installed {' '.join(packages)}\n"
    path = Path(args.path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(args.code)
    result = subprocess.run(
        [str(VENV / "python"), str(path)],
        cwd=path.parent,
        capture_output=True,
        text=True,
        timeout=getattr(args, "timeout", 120),
    )
    return f"{out}wrote {path}\nexit code {result.returncode}\n{result.stdout}{result.stderr}"
