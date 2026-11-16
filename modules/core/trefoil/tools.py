from pathlib import Path, PurePosixPath
import shutil

# The server copies this directory out of the container when the run ends.
ARTIFACTS = Path("/trefoil/artifacts")


def SaveArtifact(args):
    src = Path(args.path)
    if not src.exists():
        raise FileNotFoundError(f"no such file or directory: {src}")
    name = PurePosixPath(getattr(args, "name", None) or src.name)
    if name.is_absolute() or ".." in name.parts or not name.parts:
        raise ValueError(f"artifact name must be a relative path without '..': {name}")
    dest = ARTIFACTS / name
    dest.parent.mkdir(parents=True, exist_ok=True)
    if src.is_dir():
        shutil.copytree(src, dest, symlinks=False, dirs_exist_ok=True)
        size = sum(f.stat().st_size for f in dest.rglob("*") if f.is_file())
    else:
        shutil.copyfile(src, dest)
        size = dest.stat().st_size
    return f"saved artifact {name} ({size} bytes)"
