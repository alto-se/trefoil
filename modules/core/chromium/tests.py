# Pages are served from file:// so the tests don't need the network.

PAGE = """<!doctype html><title>Test page</title>
<p>Static text</p><p id="js"></p><a href="next.html">Local</a><a href="https://example.com/next">Next</a>
<script>document.getElementById('js').textContent = 'Rendered by JS'</script>"""


def test_open_page(tools, tmp):
    page = tmp / "index.html"
    page.write_text(PAGE)
    out = tools.OpenPage(url=page.as_uri())
    assert "title: Test page" in out, out
    assert "Static text" in out and "Rendered by JS" in out, out
    # Only http(s) links are listed.
    assert "Next: https://example.com/next" in out and "Local" not in out.split("links:")[1], out


def test_print_pdf(tools, tmp):
    page = tmp / "index.html"
    page.write_text(PAGE)
    pdf = tmp / "out" / "page.pdf"
    tools.PrintPdf(url=page.as_uri(), path=str(pdf))
    assert pdf.read_bytes().startswith(b"%PDF"), "not a PDF"
