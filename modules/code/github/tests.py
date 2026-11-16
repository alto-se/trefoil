import os
import subprocess
from unittest import SkipTest


def test_git_installed():
    subprocess.run(["git", "--version"], check=True, capture_output=True)


def test_rejects_bad_repo(tools):
    assert tools.CloneRepo(repo="not a repo").startswith("Error: repo must look like owner/name")


def test_clone_public_repo(tools):
    if not os.environ.get("GITHUB_TOKEN"):
        raise SkipTest("GITHUB_TOKEN not set")
    path = tools.CloneRepo(repo="octocat/Hello-World")
    assert path == "/workspace/repos/octocat/Hello-World", path
    assert os.path.isdir(os.path.join(path, ".git"))
