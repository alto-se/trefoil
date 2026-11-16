import subprocess


def test_go_on_path():
    out = subprocess.run(["go", "version"], capture_output=True, text=True, check=True).stdout
    assert out.startswith("go version go1."), out


def test_run_go(tools, tmp):
    code = 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("hello from go") }\n'
    out = tools.RunGo(path=str(tmp / "main.go"), code=code)
    assert "exit code 0" in out and "hello from go" in out, out


def test_run_go_compile_error(tools, tmp):
    out = tools.RunGo(path=str(tmp / "main.go"), code="package main\n\nfunc main() { nope }\n")
    assert "exit code 0" not in out, out


def test_cgo_enabled(tools, tmp):
    env = subprocess.run(["go", "env", "CGO_ENABLED"], capture_output=True, text=True, check=True).stdout
    assert env.strip() == "1", env
    code = 'package main\n\n// int add(int a, int b) { return a + b; }\nimport "C"\nimport "fmt"\n\nfunc main() { fmt.Println("cgo", C.add(2, 3)) }\n'
    out = tools.RunGo(path=str(tmp / "main.go"), code=code)
    assert "exit code 0" in out and "cgo 5" in out, out
