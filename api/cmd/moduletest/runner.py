"""Runs a module's tests.py inside the module's built image.

Usage: python /trefoil/harness/module_test.py /trefoil/tests/tests.py

Tests are plain functions named test_* in the order they are defined. A test
asks for fixtures by parameter name:

  tools  the module's tools, loaded by the harness exactly as in a run;
         tools.Write(path=..., content=...) calls the Write tool and returns
         its result
  tmp    a fresh, empty directory (pathlib.Path)

A test fails by raising (usually AssertionError) and is skipped by raising
unittest.SkipTest, e.g. when a secret it needs isn't set. The exit code is 1
if any test failed.
"""

import importlib.util
import inspect
import sys
import tempfile
import time
import traceback
import unittest
from pathlib import Path
from types import SimpleNamespace

sys.path.insert(0, "/trefoil/harness")
import main as harness  # noqa: E402


class Tools:
    def __init__(self, handlers):
        self._handlers = handlers

    def __getattr__(self, name):
        handler = self._handlers.get(name)
        if handler is None:
            raise AttributeError(f"module has no tool {name!r} (has: {', '.join(sorted(self._handlers)) or 'none'})")
        return lambda **args: handler(SimpleNamespace(**args))


def load_tests(path):
    spec = importlib.util.spec_from_file_location("module_tests", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return [(name, fn) for name, fn in vars(module).items() if name.startswith("test_") and callable(fn)]


def main():
    _, handlers = harness.load_tools(harness.load_spec())
    tools = Tools(handlers)
    tests = load_tests(sys.argv[1])
    if not tests:
        print("no tests found")
        return 0

    passed = failed = skipped = 0
    for name, fn in tests:
        start = time.monotonic()
        with tempfile.TemporaryDirectory() as tmp:
            fixtures = {"tools": tools, "tmp": Path(tmp)}
            try:
                fn(**{p: fixtures[p] for p in inspect.signature(fn).parameters})
            except unittest.SkipTest as e:
                skipped += 1
                print(f"SKIP {name}: {e}", flush=True)
                continue
            except Exception:
                failed += 1
                print(f"FAIL {name} ({time.monotonic() - start:.2f}s)", flush=True)
                print("     " + traceback.format_exc().rstrip().replace("\n", "\n     "), flush=True)
                continue
        passed += 1
        print(f"PASS {name} ({time.monotonic() - start:.2f}s)", flush=True)

    print(f"{passed} passed, {failed} failed, {skipped} skipped")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
