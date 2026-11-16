from pathlib import Path

ARTIFACTS = Path("/trefoil/artifacts")


def test_artifacts_dir_exists():
    assert ARTIFACTS.is_dir()


def test_save_file(tools, tmp):
    src = tmp / "report.txt"
    src.write_text("data")
    assert tools.SaveArtifact(path=str(src), name="tests/report.txt") == "saved artifact tests/report.txt (4 bytes)"
    assert (ARTIFACTS / "tests/report.txt").read_text() == "data"


def test_save_rejects_escaping_name(tools, tmp):
    src = tmp / "x.txt"
    src.write_text("x")
    try:
        tools.SaveArtifact(path=str(src), name="../x.txt")
    except ValueError:
        return
    raise AssertionError("expected ValueError for ../x.txt")
