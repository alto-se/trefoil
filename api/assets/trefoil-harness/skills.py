"""Agent Skills for the trefoil harness.

A skill is a folder under /trefoil/skills with a SKILL.md whose YAML front
matter gives its name and description. The harness lists the skills in the
system prompt and offers a Skill tool that loads a SKILL.md (or another file
in the folder) when the model decides it needs it.

Run as a script at build time to install a skill from a URL:

    python skills.py fetch URL [NAME]

URL is a zip of the skill folder (the shallowest SKILL.md in it marks the
folder, so GitHub archive zips work) or a SKILL.md on its own. NAME
overrides the name from the front matter.
"""

import io
import os
import re
import shutil
import sys
import tempfile
import urllib.request
import zipfile

SKILLS_DIR = "/trefoil/skills"
NAME_PATTERN = re.compile(r"^[A-Za-z0-9_.-]+$")


def parse_front_matter(text):
    """Returns the top-level string fields of a SKILL.md's front matter.

    Handles what skills use: `key: value`, quoted values, and `>`/`|` block
    scalars. Not a YAML parser; anything else is ignored.
    """
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        return {}
    fields, key, block, style = {}, None, [], None
    for line in lines[1:]:
        if line.strip() == "---":
            break
        if style and (line.startswith((" ", "\t")) or not line.strip()):
            block.append(line.strip())
            continue
        if style:
            fields[key] = (" " if style == ">" else "\n").join(block).strip()
            style, block = None, []
        m = re.match(r"^([A-Za-z0-9_-]+):\s*(.*)$", line)
        if not m:
            continue
        key, value = m.group(1), m.group(2).strip()
        if value[:1] in (">", "|"):
            style = value[0]
        elif len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            fields[key] = value[1:-1]
        else:
            fields[key] = value
    if style:
        fields[key] = (" " if style == ">" else "\n").join(block).strip()
    return fields


def discover(root=SKILLS_DIR):
    """Returns [{name, description, path}] for the skills installed under root."""
    if not os.path.isdir(root):
        return []
    skills = []
    for entry in sorted(os.listdir(root)):
        path = os.path.join(root, entry)
        skill_md = os.path.join(path, "SKILL.md")
        if not os.path.isfile(skill_md):
            continue
        with open(skill_md) as f:
            meta = parse_front_matter(f.read())
        skills.append({"name": entry, "description": meta.get("description", ""), "path": path})
    return skills


def read_file(skill, rel):
    """Reads a file in a skill's folder, refusing paths that leave it."""
    root = os.path.realpath(skill["path"])
    path = os.path.realpath(os.path.join(root, rel or "SKILL.md"))
    if os.path.commonpath([root, path]) != root:
        raise ValueError(f"{rel!r} is outside the skill folder")
    if os.path.isdir(path):
        return "\n".join(listing(path, root))
    with open(path) as f:
        return f.read()


def listing(path, root):
    """Paths of the files under path, relative to root."""
    out = []
    for dirpath, dirnames, filenames in os.walk(path):
        dirnames[:] = sorted(d for d in dirnames if not d.startswith(".") and d != "__pycache__")
        out += sorted(os.path.relpath(os.path.join(dirpath, f), root) for f in filenames if not f.startswith("."))
    return out


def fetch(url, name=None, root=SKILLS_DIR):
    with urllib.request.urlopen(url, timeout=120) as resp:
        data = resp.read()
    with tempfile.TemporaryDirectory() as tmp:
        if zipfile.is_zipfile(io.BytesIO(data)):
            zipfile.ZipFile(io.BytesIO(data)).extractall(tmp)
            found = sorted((p for p, _, files in os.walk(tmp) if "SKILL.md" in files), key=lambda p: p.count(os.sep))
            if not found:
                sys.exit(f"skills: no SKILL.md in {url}")
            src = found[0]
        else:
            src = tmp
            with open(os.path.join(tmp, "SKILL.md"), "wb") as f:
                f.write(data)
        with open(os.path.join(src, "SKILL.md")) as f:
            name = name or parse_front_matter(f.read()).get("name")
        if not name or not NAME_PATTERN.match(name):
            sys.exit(f"skills: {url}: no usable name ({name!r}); set `name` on the skill")
        dest = os.path.join(root, name)
        if os.path.exists(dest):
            sys.exit(f"skills: {url}: skill {name!r} is already installed")
        os.makedirs(root, exist_ok=True)
        shutil.copytree(src, dest)
    print(f"skills: installed {name} from {url}")


if __name__ == "__main__":
    if len(sys.argv) not in (3, 4) or sys.argv[1] != "fetch":
        sys.exit("usage: skills.py fetch URL [NAME]")
    fetch(sys.argv[2], sys.argv[3] if len(sys.argv) == 4 else None)
