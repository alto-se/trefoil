import os
import subprocess
from unittest import SkipTest


def test_psql_installed():
    out = subprocess.run(["psql", "--version"], capture_output=True, text=True, check=True).stdout
    assert out.startswith("psql (PostgreSQL)"), out


def need_db():
    if not os.environ.get("POSTGRES_URL"):
        raise SkipTest("POSTGRES_URL not set")


def test_select(tools):
    need_db()
    out = tools.Query(sql="SELECT 40 + 2 AS answer;")
    assert "exit code 0" in out and "42" in out, out


def test_meta_command(tools):
    need_db()
    out = tools.Query(sql="\\dn")
    assert "exit code 0" in out and "public" in out, out


def test_read_only_by_default(tools):
    need_db()
    out = tools.Query(sql="CREATE TEMP TABLE t (x int);")
    assert "exit code 0" not in out and "read-only" in out, out


def test_write(tools):
    need_db()
    out = tools.Query(sql="CREATE TEMP TABLE t (x int); INSERT INTO t VALUES (7); SELECT sum(x) FROM t;", write=True)
    assert "exit code 0" in out and "7" in out, out


def test_sql_error(tools):
    need_db()
    out = tools.Query(sql="SELECT * FROM no_such_table;")
    assert "exit code 0" not in out and "no_such_table" in out, out
