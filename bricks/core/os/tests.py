def test_write_then_read(tools, tmp):
    path = tmp / "sub" / "note.txt"
    assert tools.Write(path=str(path), content="hello") == f"Wrote 5 characters to {path}"
    assert tools.Read(path=str(path)) == "hello"


def test_bash_reports_exit_code_and_output(tools):
    out = tools.Bash(command="echo hi; echo oops >&2; exit 3")
    assert out.startswith("exit code 3\n"), out
    assert "hi" in out and "oops" in out
