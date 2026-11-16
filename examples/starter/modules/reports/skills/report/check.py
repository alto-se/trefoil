"""Checks a Markdown report against template.md: a title, the template's
sections in order, no leftover placeholders, 3-5 key points and a word limit."""
import argparse
import re
import sys

SECTIONS = ["Summary", "Key points", "Next steps"]


def check(text, words):
    problems = []
    if not re.match(r"# \S", text):
        problems.append("first line should be a '# Title' heading")
    found = re.findall(r"^## (.+?)\s*$", text, re.M)
    if found != SECTIONS:
        problems.append(f"sections should be {SECTIONS}, found {found}")
    for p in sorted(set(re.findall(r"<[A-Z][^<>\n]*>", text))):
        problems.append(f"placeholder left in: {p}")
    points = re.search(r"^## Key points\s*$(.*?)(?=^## |\Z)", text, re.M | re.S)
    if points:
        n = len(re.findall(r"^[-*] ", points.group(1), re.M))
        if not 3 <= n <= 5:
            problems.append(f"key points should have 3-5 bullets, found {n}")
    body = re.sub(r"^#.*$", "", text, flags=re.M)
    count = len(re.findall(r"\w+(?:'\w+)?", body))
    if words and count > words:
        problems.append(f"body is {count} words, limit is {words}")
    return problems


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("path")
    parser.add_argument("--words", type=int, default=0, help="word limit for the body (0: none)")
    args = parser.parse_args()
    with open(args.path) as f:
        problems = check(f.read(), args.words)
    for p in problems:
        print(p)
    if problems:
        sys.exit(1)
    print("ok")


if __name__ == "__main__":
    main()
