from collections import Counter
from pathlib import Path
import re

WORDS = re.compile(r"[A-Za-z]+(?:'[A-Za-z]+)?")

def TextStats(args):
    words = WORDS.findall(args.text)
    sentences = [s for s in re.split(r"[.!?]+", args.text) if s.strip()]
    minutes = len(words) / 230
    return (
        f"words: {len(words)}\n"
        f"sentences: {len(sentences)}\n"
        f"characters: {len(args.text)}\n"
        f"avg words per sentence: {len(words) / max(len(sentences), 1):.1f}\n"
        f"reading time: {max(round(minutes), 1)} min"
    )

def SpellCheck(args):
    known = {w.lower() for w in Path("/usr/share/dict/words").read_text().split()}
    unknown = Counter(w for w in WORDS.findall(args.text) if w.lower() not in known)
    if not unknown:
        return "no unknown words"
    return "\n".join(f"{w} ({n})" for w, n in unknown.most_common())
