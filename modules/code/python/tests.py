import subprocess


def test_venv_python():
    out = subprocess.run(["/opt/venv/bin/python", "-c", "import sys; print(sys.prefix)"], capture_output=True, text=True, check=True)
    assert out.stdout.strip() == "/opt/venv", out


def test_run_python(tools, tmp):
    out = tools.RunPython(path=str(tmp / "main.py"), code="print(6 * 7)")
    assert out.splitlines()[1:3] == ["exit code 0", "42"], out


def test_run_python_error(tools, tmp):
    out = tools.RunPython(path=str(tmp / "main.py"), code="raise ValueError('boom')")
    assert "exit code 1" in out and "boom" in out, out


def test_run_python_packages(tools, tmp):
    code = "import six\nprint('six', six.__version__)"
    out = tools.RunPython(path=str(tmp / "main.py"), code=code, packages=["six"])
    assert out.startswith("installed six") and "exit code 0" in out and "six " in out, out


def test_bad_package(tools, tmp):
    out = tools.RunPython(path=str(tmp / "main.py"), code="print(1)", packages=["trefoil-no-such-package-xyz"])
    assert out.startswith("pip install failed"), out
