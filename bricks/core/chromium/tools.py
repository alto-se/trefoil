from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import urljoin
import os
import signal
import subprocess
import tempfile
import time

MAX_TEXT = 8000
MAX_LINKS = 60


def chromium(*args, timeout=60, done=None):
    """Runs headless Chromium and returns its stdout. Chromium sometimes hangs on exit after
    finishing its work (seen after --print-to-pdf), so `done(stderr)` lets the caller say the
    work is finished; the process is then killed instead of waited for."""
    # --no-sandbox: the container runs as root, which Chromium's sandbox refuses.
    cmd = ["chromium", "--headless", "--no-sandbox", "--disable-gpu", "--disable-dev-shm-usage",
           "--hide-scrollbars", "--virtual-time-budget=5000", *args]
    # Temp files rather than pipes: a large DOM would fill a pipe while we poll.
    with tempfile.TemporaryFile("w+") as out, tempfile.TemporaryFile("w+") as err:
        proc = subprocess.Popen(cmd, stdout=out, stderr=err, text=True, start_new_session=True)
        deadline, finished = time.monotonic() + timeout, False
        try:
            while proc.poll() is None:
                err.seek(0)
                if done and done(err.read()):
                    finished = True
                    break
                if time.monotonic() > deadline:
                    raise TimeoutError(f"chromium did not finish within {timeout}s")
                time.sleep(0.25)
        finally:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
        out.seek(0)
        err.seek(0)
        stdout, stderr = out.read(), err.read()
    if proc.returncode != 0 and not finished:
        raise RuntimeError(f"chromium exited {proc.returncode}: {stderr.strip()[-2000:]}")
    return stdout


class PageParser(HTMLParser):
    """Collects the title, visible text and links of a rendered page."""

    SKIP = {"script", "style", "noscript", "template", "svg"}

    def __init__(self, base):
        super().__init__()
        self.base, self.title, self.text, self.links = base, "", [], []
        self.skipping, self.in_title, self.href, self.link_text = 0, False, None, []

    def handle_starttag(self, tag, attrs):
        if tag == "title":
            self.in_title = True
        elif tag in self.SKIP:
            self.skipping += 1
        elif tag == "a":
            self.href, self.link_text = dict(attrs).get("href"), []

    def handle_endtag(self, tag):
        if tag == "title":
            self.in_title = False
        elif tag in self.SKIP:
            self.skipping = max(0, self.skipping - 1)
        elif tag == "a" and self.href:
            href = urljoin(self.base, self.href)
            if href.startswith(("http://", "https://")):
                self.links.append((" ".join(self.link_text), href))
            self.href = None

    def handle_data(self, data):
        data = data.strip()
        if self.in_title:
            self.title += data
        elif data and not self.skipping:
            self.text.append(data)
            if self.href:
                self.link_text.append(data)


def OpenPage(args):
    parser = PageParser(args.url)
    parser.feed(chromium("--dump-dom", args.url))
    text = " ".join(parser.text)
    if len(text) > MAX_TEXT:
        text = text[:MAX_TEXT] + " …(truncated)"
    links = "\n".join(f"- {label or '(no text)'}: {href}" for label, href in parser.links[:MAX_LINKS])
    return f"title: {parser.title}\nurl: {args.url}\n\ntext:\n{text}\n\nlinks:\n{links or '(none)'}"


def PrintPdf(args):
    path = Path(args.path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.unlink(missing_ok=True)
    chromium("--no-pdf-header-footer", f"--print-to-pdf={path}", args.url,
             done=lambda stderr: f"written to file {path}" in stderr)
    if not path.exists():
        raise RuntimeError(f"chromium did not write {path}")
    return f"wrote {path} ({path.stat().st_size} bytes) from {args.url}"
