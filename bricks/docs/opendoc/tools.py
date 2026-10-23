from pathlib import Path
import base64
import mimetypes
import re
import shutil
import subprocess
import tempfile
import xml.etree.ElementTree as ET
import zipfile

MAX_TEXT = 30000
MAX_REPEAT = 50  # repeated rows/columns past this are cut (spreadsheets repeat empty cells to the sheet's edge)
ODF = {".odt", ".ods", ".odp", ".odg"}
FORMATS = {"odt", "ods", "odp", "pdf", "docx", "xlsx", "pptx", "html", "txt", "csv", "png"}

NS = {
    "office": "urn:oasis:names:tc:opendocument:xmlns:office:1.0",
    "text": "urn:oasis:names:tc:opendocument:xmlns:text:1.0",
    "table": "urn:oasis:names:tc:opendocument:xmlns:table:1.0",
    "draw": "urn:oasis:names:tc:opendocument:xmlns:drawing:1.0",
    "presentation": "urn:oasis:names:tc:opendocument:xmlns:presentation:1.0",
}


def q(name):
    prefix, local = name.split(":")
    return f"{{{NS[prefix]}}}{local}"


def soffice(*args, timeout=180):
    """Runs headless LibreOffice with a throwaway profile, so parallel calls don't fight over the
    profile lock, and returns its output."""
    with tempfile.TemporaryDirectory() as profile:
        result = subprocess.run(
            ["soffice", f"-env:UserInstallation=file://{profile}", "--headless", "--norestore", *args],
            capture_output=True, text=True, timeout=timeout,
        )
    if result.returncode != 0:
        raise RuntimeError(f"soffice exited {result.returncode}: {(result.stdout + result.stderr).strip()[-2000:]}")
    return result.stdout + result.stderr


def convert(src, fmt, out_dir):
    """Converts src with LibreOffice and returns the files it wrote. CSV from a spreadsheet writes
    one file per sheet (<name>-<sheet>.csv)."""
    args = []
    if src.suffix.lower() in {".html", ".htm"}:
        # Open HTML as a Writer document: Writer/Web, the default, only saves html and odt.
        args = ["--infilter=HTML (StarWriter)"]
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    before = set(out_dir.iterdir())
    target = fmt
    if fmt == "csv":
        # Comma, double quotes, UTF-8, first line 1, ..., -1 = every sheet to its own file.
        target = 'csv:Text - txt - csv (StarCalc):44,34,76,1,,0,false,true,false,false,false,-1'
    elif fmt == "txt":
        target = "txt:Text (encoded):UTF8"
    soffice(*args, "--convert-to", target, "--outdir", str(out_dir), str(src))
    written = sorted(set(out_dir.iterdir()) - before)
    if not written:
        stem = out_dir / f"{src.stem}.{fmt}"
        if stem.exists():  # overwrote an earlier conversion
            written = [stem]
        else:
            raise RuntimeError(f"LibreOffice did not convert {src} to {fmt}")
    return written


def text_of(el):
    """Text of a paragraph-like element, with ODF's space, tab and line-break elements expanded."""
    out = [el.text or ""]
    for child in el:
        if child.tag == q("text:s"):
            out.append(" " * int(child.get(q("text:c"), "1")))
        elif child.tag == q("text:tab"):
            out.append("\t")
        elif child.tag == q("text:line-break"):
            out.append("\n")
        elif child.tag in (q("text:note"), q("office:annotation")):
            pass
        else:
            out.append(text_of(child))
        out.append(child.tail or "")
    return "".join(out)


def cell_text(cell):
    value = "\n".join(text_of(p) for p in cell.findall(q("text:p")))
    formula = cell.get(q("table:formula"))
    return f"{value} {{{formula}}}" if formula else value


def table_lines(table):
    rows = []
    for row in table.iter(q("table:table-row")):
        cells = []
        for cell in row:
            if cell.tag not in (q("table:table-cell"), q("table:covered-table-cell")):
                continue
            n = int(cell.get(q("table:number-columns-repeated"), "1"))
            cells += [cell_text(cell).replace("\n", " ").replace("|", "\\|")] * min(n, MAX_REPEAT)
        while cells and not cells[-1]:
            cells.pop()
        n = int(row.get(q("table:number-rows-repeated"), "1"))
        rows += [cells] * min(n, MAX_REPEAT)
    while rows and not rows[-1]:
        rows.pop()
    return ["| " + " | ".join(r) + " |" if r else "|" for r in rows]


def block_lines(el, depth=0):
    """Markdown-ish lines for a block element and its children."""
    tag = el.tag
    if tag == q("text:h"):
        return ["#" * int(el.get(q("text:outline-level"), "1")) + " " + text_of(el), ""]
    if tag == q("text:p"):
        text = text_of(el)
        return [text, ""] if text.strip() else []
    if tag == q("text:list"):
        lines = []
        for item in el:
            first = True
            for child in item:
                if child.tag == q("text:list"):
                    lines += block_lines(child, depth + 1)  # already indented and bulleted
                    continue
                for l in block_lines(child, depth + 1):
                    if l:
                        lines.append("  " * depth + ("- " if first else "  ") + l)
                        first = False
        return lines + ([""] if depth == 0 else [])
    if tag == q("table:table"):
        name = el.get(q("table:name"), "")
        return [f"[table {name}]"] + table_lines(el) + [""]
    lines = []
    for child in el:
        lines += block_lines(child, depth)
    return lines


def odf_text(path):
    with zipfile.ZipFile(path) as z:
        root = ET.fromstring(z.read("content.xml"))
    body = root.find(q("office:body"))
    lines = []
    for kind in body:
        if kind.tag == q("office:spreadsheet"):
            for sheet in kind.findall(q("table:table")):
                lines += [f"## Sheet: {sheet.get(q('table:name'))}"] + table_lines(sheet) + [""]
        elif kind.tag in (q("office:presentation"), q("office:drawing")):
            for i, page in enumerate(kind.findall(q("draw:page")), 1):
                lines.append(f"## Slide {i}: {page.get(q('draw:name'), '')}")
                for child in page:
                    if child.tag == q("presentation:notes"):
                        notes = [l for c in child for l in block_lines(c) if l.strip()]
                        if notes:
                            lines.append("Notes: " + " ".join(notes))
                    else:
                        lines += block_lines(child)
                lines.append("")
        else:
            lines += block_lines(kind)
    return "\n".join(lines).strip() + "\n"


IMG_SRC = re.compile(r"""(<img\b[^>]*?\bsrc\s*=\s*)(["'])(?:file://)?(/[^"']+)\2""", re.I)


def inline_images(html):
    """Turns <img src="/abs/path"> into data: URIs; LibreOffice would only link files, not embed them."""
    def sub(m):
        path = Path(m[3])
        if not path.is_file():
            raise FileNotFoundError(f"image {path} not found")
        mime = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        return f'{m[1]}{m[2]}data:{mime};base64,{base64.b64encode(path.read_bytes()).decode()}{m[2]}'
    return IMG_SRC.sub(sub, html)


def ReadDocument(args):
    path = Path(args.path)
    if not path.exists():
        raise FileNotFoundError(path)
    if path.suffix.lower() in ODF:
        text = odf_text(path)
    else:
        # docx, xlsx, pptx, doc, rtf, ...: convert to the matching OpenDocument format first.
        fmt = {".xlsx": "ods", ".xls": "ods", ".csv": "ods", ".pptx": "odp", ".ppt": "odp"}.get(path.suffix.lower(), "odt")
        with tempfile.TemporaryDirectory() as tmp:
            text = odf_text(convert(path, fmt, tmp)[0])
    limit = int(getattr(args, "max_chars", None) or MAX_TEXT)
    if len(text) > limit:
        text = text[:limit] + f"\n…(truncated, {len(text)} characters in total)"
    return text


def ConvertDocument(args):
    path, fmt = Path(args.path), args.format.lower().lstrip(".")
    if fmt not in FORMATS:
        raise ValueError(f"unsupported format {fmt!r}; use one of {', '.join(sorted(FORMATS))}")
    if not path.exists():
        raise FileNotFoundError(path)
    out_dir = Path(getattr(args, "out_dir", None) or path.parent)
    files = convert(path, fmt, out_dir)
    return "\n".join(f"wrote {f} ({f.stat().st_size} bytes)" for f in files)


def CreateDocument(args):
    path = Path(args.path)
    fmt = path.suffix.lower().lstrip(".")
    source = (getattr(args, "source_format", None) or ("csv" if fmt in {"ods", "xlsx"} else "html")).lower()
    targets = {"html": {"odt", "docx", "pdf"}, "csv": {"ods", "xlsx", "pdf"}}
    if source not in targets:
        raise ValueError("source_format must be html or csv")
    if fmt not in targets[source]:
        raise ValueError(f"can't create .{fmt} from {source}; use {', '.join('.' + t for t in sorted(targets[source]))}")
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / f"{path.stem}.{source}"
        src.write_text(inline_images(args.content) if source == "html" else args.content)
        out = convert(src, fmt, Path(tmp) / "out")[0]
        shutil.move(out, path)
    return f"wrote {path} ({path.stat().st_size} bytes)"
