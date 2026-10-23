import json
import subprocess
import zipfile

ODF_PY = "/opt/odf/bin/python"
SKILLS = "/trefoil/skills"

HTML = """<h1>Report</h1><p>Hello <b>world</b></p>
<ul><li>one</li><li>two<ul><li>nested</li></ul></li></ul>
<table border="1"><tr><th>a</th><th>b</th></tr><tr><td>1</td><td>2</td></tr></table>"""


def script(skill, name, *args):
    result = subprocess.run([ODF_PY, f"{SKILLS}/{skill}/scripts/{name}", *map(str, args)],
                            capture_output=True, text=True, timeout=300)
    return result.returncode, result.stdout + result.stderr


def test_html_to_odt_and_back(tools, tmp):
    path = tmp / "report.odt"
    assert "wrote" in tools.CreateDocument(path=str(path), content=HTML)
    text = tools.ReadDocument(path=str(path))
    assert "# Report" in text and "Hello world" in text, text
    assert "- two\n  - nested" in text, text
    assert "| a | b |" in text and "| 1 | 2 |" in text, text


def test_html_to_docx_and_pdf(tools, tmp):
    docx, pdf = tmp / "r.docx", tmp / "r.pdf"
    tools.CreateDocument(path=str(docx), content=HTML)
    assert "# Report" in tools.ReadDocument(path=str(docx))
    tools.CreateDocument(path=str(pdf), content=HTML)
    assert pdf.read_bytes().startswith(b"%PDF")


def test_local_images_are_embedded(tools, tmp):
    png = tmp / "dot.png"
    png.write_bytes(bytes.fromhex(
        "89504e470d0a1a0a0000000d4948445200000001000000010806000000"
        "1f15c4890000000d49444154789c6360000002000154a24f5d0000000049454e44ae426082"))
    path = tmp / "img.odt"
    tools.CreateDocument(path=str(path), content=f'<p><img src="{png}" width="10" height="10"></p>')
    assert any(n.startswith("Pictures/") for n in zipfile.ZipFile(path).namelist())


def test_csv_to_ods_and_csv_per_sheet(tools, tmp):
    ods = tmp / "data.ods"
    tools.CreateDocument(path=str(ods), content="name,qty\napple,3\npear,4\n")
    assert "| apple | 3 |" in tools.ReadDocument(path=str(ods))
    out = tools.ConvertDocument(path=str(ods), format="csv", out_dir=str(tmp / "csv"))
    assert "data-data.csv" in out, out
    assert (tmp / "csv" / "data-data.csv").read_text().splitlines()[1] == "apple,3"


def test_spreadsheet_script_calculates_formulas(tools, tmp):
    spec = tmp / "spec.json"
    spec.write_text(json.dumps({"sheets": [
        {"name": "Budget", "header": True, "columns": [4, 3], "rows": [
            ["Item", "Cost", "Share"],
            ["Rent", {"currency": 100, "code": "SEK"}, {"formula": "=B2/B$4", "type": "percent"}],
            ["Food", {"currency": 300, "code": "SEK"}, {"formula": "=B3/B$4", "type": "percent"}],
            ["Total", {"formula": "=SUM(B2:B3)", "type": "currency", "code": "SEK"},
             {"formula": "=SUM(C2:C3)", "type": "percent"}],
            ["When", {"date": "2026-10-22"}, '=IF(B2>50,"big, yes","small")'],
        ]},
        {"name": "Other sheet", "rows": [["=Budget!B4*2"]]},
    ]}))
    code, out = script("odf-spreadsheet", "make_ods.py", spec, tmp / "b.ods")
    assert code == 0, out
    text = tools.ReadDocument(path=str(tmp / "b.ods"))
    assert "| Total | 400.00 SEK {of:=SUM([.B2:.B3])} | 100.0% " in text, text
    assert "25.0%" in text and "2026-10-22" in text and "big, yes" in text, text
    assert "800" in text and "{of:=[$Budget.B4]*2}" in text, text
    tools.ConvertDocument(path=str(tmp / "b.ods"), format="xlsx")
    assert "400" in tools.ReadDocument(path=str(tmp / "b.xlsx"))


def test_presentation_script_and_pptx(tools, tmp):
    deck = tmp / "deck.json"
    deck.write_text(json.dumps({"slides": [
        {"title": "Review", "subtitle": "Q3"},
        {"title": "Highlights", "bullets": ["Up 12%", ["EU", "US"], "Two customers"], "notes": "Say hi."},
    ]}))
    code, out = script("odf-presentation", "make_odp.py", deck, tmp / "d.odp")
    assert code == 0, out
    text = tools.ReadDocument(path=str(tmp / "d.odp"))
    assert "## Slide 2" in text and "- Up 12%\n  - EU" in text and "Notes: Say hi." in text, text
    tools.ConvertDocument(path=str(tmp / "d.odp"), format="pptx")
    assert "Two customers" in tools.ReadDocument(path=str(tmp / "d.pptx"))


def test_fill_template(tools, tmp):
    tpl = tmp / "tpl.odt"
    tools.CreateDocument(path=str(tpl), content="<h1>Offer for {{customer}}</h1><p>Dear <b>{{contact}}</b>, {{cus<i>tomer</i>}}.</p><p>{{left}}</p>")
    values = tmp / "v.json"
    values.write_text(json.dumps({"customer": "Acme", "contact": "Ada"}))
    code, out = script("odf-text", "fill_template.py", tpl, values, tmp / "out.odt")
    assert code == 1 and "no value for: left" in out, out
    text = tools.ReadDocument(path=str(tmp / "out.odt"))
    assert "# Offer for Acme" in text and "Dear Ada, Acme." in text and "{{left}}" in text, text
