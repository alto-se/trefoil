def test_run_node(tools, tmp):
    out = tools.RunNode(path=str(tmp / "main.js"), code="console.log(6 * 7)")
    assert out.splitlines()[1:3] == ["exit code 0", "42"], out


def test_run_node_error(tools, tmp):
    out = tools.RunNode(path=str(tmp / "main.js"), code="throw new Error('boom')")
    assert "exit code 1" in out and "boom" in out, out


def test_npm_on_path():
    import subprocess
    out = subprocess.run(["npm", "--version"], capture_output=True, text=True, check=True)
    assert out.stdout.strip() and "does not support" not in out.stderr, out
