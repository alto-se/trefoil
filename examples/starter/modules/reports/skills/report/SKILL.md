---
name: report
description: Template and checker for short Markdown reports and briefings. Use when writing a report, briefing or summary to a .md file.
---

# Writing a report

Start from `template.md` in this folder (read it with the Skill tool,
`file: template.md`). Keep its headings, in order, and replace every
placeholder in angle brackets.

- The title is a plain statement of the subject, not a question.
- **Summary** is two or three sentences a reader can stop after.
- **Key points** is three to five bullets, one idea each.
- **Next steps** lists concrete actions, or `None.` if there are none.
- Write for the stated audience: define terms they may not know, skip ones they do.

Check the file before handing it over:

```sh
python3 /trefoil/skills/report/check.py REPORT.md [--words 300]
```

It prints `ok` and exits 0, or lists each problem and exits 1.
