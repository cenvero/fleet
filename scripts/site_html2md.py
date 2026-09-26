#!/usr/bin/env python3
"""Convert the fleet.cenvero.org HTML pages to clean Markdown mirrors.

Usage: python3 scripts/site_html2md.py <page.html> <canonical-url> [--title "Override H1"] [--shift N]
(normally run through scripts/build-site-llms.py)

Drops site chrome (header, footer, sidebar, breadcrumbs, buttons, icons,
eyebrows) and renders the main content as GitHub-flavoured Markdown with
absolute links. Stdlib only.
"""
import re
import sys
from html.parser import HTMLParser
from urllib.parse import urljoin

VOID = {"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta",
        "source", "track", "wbr"}
SKIP_TAGS = {"script", "style", "button", "template", "head", "input", "label"}
SKIP_CLASSES = {"site-header", "site-footer", "skip-link", "docs-sidebar", "breadcrumbs",
                "card-icon", "window-bar", "copy-btn", "tabs", "showcase-tabs",
                "visually-hidden", "docs-toggle", "edit-link", "eyebrow", "pill",
                "docs-nav-empty", "dots", "copy-label"}
BLOCK_TAGS = {"p", "div", "section", "article", "main", "ul", "ol", "li", "table", "pre",
              "h1", "h2", "h3", "h4", "h5", "h6", "figure", "figcaption", "details",
              "summary", "blockquote", "nav", "header", "footer", "aside", "dl", "dt",
              "dd", "hr", "body", "html"}


class Node:
    def __init__(self, tag, attrs, parent=None):
        self.tag, self.attrs, self.parent, self.children = tag, dict(attrs), parent, []

    @property
    def classes(self):
        return set(self.attrs.get("class", "").split())

    def find_all(self, pred):
        for c in self.children:
            if isinstance(c, Node):
                if pred(c):
                    yield c
                yield from c.find_all(pred)

    def text(self):
        out = []
        for c in self.children:
            out.append(c if isinstance(c, str) else c.text())
        return "".join(out)


class Builder(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.root = Node("#root", {})
        self.cur = self.root

    def handle_starttag(self, tag, attrs):
        n = Node(tag, attrs, self.cur)
        self.cur.children.append(n)
        if tag not in VOID:
            self.cur = n

    def handle_startendtag(self, tag, attrs):
        self.cur.children.append(Node(tag, attrs, self.cur))

    def handle_endtag(self, tag):
        n = self.cur
        while n is not self.root and n.tag != tag:
            n = n.parent
        if n is not self.root:
            self.cur = n.parent

    def handle_data(self, data):
        self.cur.children.append(data)


def skipped(n):
    if n.tag in SKIP_TAGS:
        return True
    if n.classes & SKIP_CLASSES:
        return True
    if n.tag == "svg" and n.attrs.get("role") != "img":
        return True
    return False


def is_block(n):
    if isinstance(n, str):
        return False
    if n.tag in BLOCK_TAGS:
        return True
    if n.tag == "svg" and n.attrs.get("role") == "img":
        return True
    return False


class Renderer:
    def __init__(self, base, shift=0):
        self.base = base
        self.shift = shift
        self.last_level = 1

    # ---------- inline ----------
    def inline(self, nodes, in_code=False):
        out = []
        for c in nodes:
            if isinstance(c, str):
                out.append(c if in_code else re.sub(r"\s+", " ", c))
                continue
            if skipped(c) or c.tag == "svg" or c.tag == "img":
                continue
            if c.tag in ("code", "kbd"):
                t = re.sub(r"\s+", " ", c.text())
                fence = "``" if "`" in t else "`"
                pad = " " if "`" in t else ""
                out.append(f"{fence}{pad}{t}{pad}{fence}")
            elif c.tag in ("strong", "b"):
                t = self.inline(c.children).strip()
                if t:
                    out.append(f"**{t}**")
            elif c.tag in ("em", "i"):
                t = self.inline(c.children).strip()
                if t:
                    out.append(f"*{t}*")
            elif c.tag == "a":
                t = self.inline(c.children).strip()
                href = c.attrs.get("href", "")
                if not t:
                    continue
                if href and not href.startswith("mailto:"):
                    href = urljoin(self.base, href)
                out.append(f"[{t}]({href})" if href else t)
            elif c.tag == "br":
                out.append("  \n")
            elif c.tag == "span" and "tag" in c.classes:
                out.append(f" ({self.inline(c.children).strip()})")
            elif c.tag == "span" and "muted" in c.classes and c.text().strip() == "—":
                out.append("—")
            else:
                out.append(self.inline(c.children, in_code))
        res = "".join(out)
        return res if in_code else re.sub(r"(?<=\S) {2,}(?=\S)", " ", res)

    # ---------- blocks ----------
    def heading(self, level, text):
        level = max(1, min(6, level + self.shift))
        return "#" * level + " " + text.strip()

    def blocks(self, node, ctx=None):
        """Render the children of node as a list of Markdown blocks."""
        ctx = ctx or {}
        out, buf = [], []

        def flush():
            t = self.inline(buf).strip()
            if t:
                out.append(t)
            buf.clear()

        for c in node.children:
            if isinstance(c, Node) and skipped(c):
                continue
            if is_block(c):
                flush()
                out.extend(self.block(c, ctx))
            else:
                buf.append(c)
        flush()
        return [b for b in out if b.strip()]

    def block(self, n, ctx):
        tag, cls = n.tag, n.classes
        if re.fullmatch(r"h[1-6]", tag):
            level = int(tag[1])
            text = self.inline(n.children).strip()
            if ctx.get("in_list"):
                return [f"**{text}**"]
            self.last_level = level
            return [self.heading(level, text)]
        if tag == "p":
            t = self.inline(n.children).strip()
            return [t] if t else []
        if tag == "pre":
            return [self.fence(n.text(), "text")]
        if tag in ("ul", "ol"):
            return [self.list_block(n, ordered=(tag == "ol"), ctx=ctx)]
        if tag == "table":
            return [self.table(n)]
        if tag == "details":
            summary = next((c for c in n.children if isinstance(c, Node) and c.tag == "summary"), None)
            res = []
            if summary is not None:
                level = min(6, self.last_level + 1)
                text = self.inline(summary.children).strip()
                res.append(f"**{text}**" if ctx.get("in_list") else self.heading(level, text))
            body = Node("div", {})
            body.children = [c for c in n.children if c is not summary]
            res.extend(self.blocks(body, ctx))
            return res
        if tag == "figure":
            res = []
            for img in n.find_all(lambda x: x.tag == "img"):
                res.append(f"![{img.attrs.get('alt', '')}]({urljoin(self.base, img.attrs.get('src', ''))})")
            cap = next(n.find_all(lambda x: x.tag == "figcaption"), None)
            if cap is not None:
                res.append(self.inline(cap.children).strip())
            return res
        if tag == "svg":  # role=img diagram
            title = next(n.find_all(lambda x: x.tag == "title"), None)
            desc = next(n.find_all(lambda x: x.tag == "desc"), None)
            parts = [x.text().strip() for x in (title, desc) if x is not None]
            return [f"*Diagram — {': '.join(parts)}*"] if parts else []
        if "window" in cls:  # terminal illustration
            pre = next(n.find_all(lambda x: x.tag == "pre"), None)
            res = []
            if n.attrs.get("aria-label"):
                res.append(f"*Example — {n.attrs['aria-label']}*")
            if pre is not None:
                res.append(self.fence(pre.text(), "console"))
            return res
        if tag == "div" and "code" in cls:
            pre = next(n.find_all(lambda x: x.tag == "pre"), None)
            has_copy = any(True for _ in n.find_all(lambda x: "copy-btn" in x.classes))
            return [self.fence(pre.text(), "sh" if has_copy else "text")] if pre is not None else []
        if tag == "div" and ("cmd" in cls or "mini-code" in cls):
            code = next(n.find_all(lambda x: x.tag == "code"), None)
            text = code.text() if code is not None else n.text()
            return [self.fence(text, "powershell" if "ps" in cls else "sh")]
        if tag == "div" and "cmd-label" in cls:
            return [self.inline(n.children).strip() + ":"]
        if tag == "div" and "tabpanel" in cls:
            label = n.attrs.get("aria-labelledby")
            res = []
            if label:
                root = n
                while root.parent is not None:
                    root = root.parent
                tab = next(root.find_all(lambda x: x.attrs.get("id") == label), None)
                if tab is not None and "tab" in tab.classes:
                    res.append(f"**{self.inline(tab.children).strip()}**")
            res.extend(self.blocks(n, ctx))
            return res
        if tag == "div" and "callout" in cls:
            inner = self.blocks(n, ctx)
            text = "\n\n".join(inner)
            return ["\n".join("> " + line if line else ">" for line in text.split("\n"))]
        if tag == "div" and "stats" in cls:
            items = []
            for st in n.find_all(lambda x: "stat" in x.classes):
                strong = next(st.find_all(lambda x: x.tag == "strong"), None)
                span = next(st.find_all(lambda x: x.tag == "span"), None)
                items.append(f"- **{strong.text().strip()}** {span.text().strip()}")
            return ["\n".join(items)]
        if tag == "div" and "hero-cta" in cls:
            links = [self.inline([a]).strip() for a in n.find_all(lambda x: x.tag == "a")]
            links = [l for l in links if l]
            return [" · ".join(links)] if links else []
        if tag == "div" and "release-head" in cls:
            res = []
            meta = []
            for c in n.children:
                if not isinstance(c, Node):
                    continue
                if re.fullmatch(r"h[1-6]", c.tag):
                    res.extend(self.block(c, ctx))
                elif c.tag == "time":
                    meta.append(f"Released {c.attrs.get('datetime') or c.text().strip()}")
                elif "tag" in c.classes:
                    meta.append(c.text().strip())
            if meta:
                res.append("*" + " · ".join(meta) + "*")
            return res
        return self.blocks(n, ctx)

    def fence(self, text, lang):
        text = text.strip("\n")
        # dedent common leading whitespace
        lines = text.split("\n")
        indents = [len(l) - len(l.lstrip(" ")) for l in lines if l.strip()]
        cut = min(indents) if indents else 0
        text = "\n".join(l[cut:].rstrip() for l in lines)
        first_line = text.split("\n", 1)[0]
        m = re.match(r"#\s+\S+\.(ya?ml|toml|json)\s*$", first_line)
        if m and lang == "sh":
            lang = {"yml": "yaml"}.get(m.group(1), m.group(1))
        ticks = "````" if "```" in text else "```"
        return f"{ticks}{lang}\n{text}\n{ticks}"

    def list_block(self, n, ordered, ctx):
        lines = []
        i = 0
        loose = any(isinstance(li, Node) and li.tag == "li" and len(self.blocks(li, {**ctx, "in_list": True})) > 1
                    for li in n.children)
        for li in n.children:
            if not isinstance(li, Node) or li.tag != "li" or skipped(li):
                continue
            i += 1
            marker = f"{i}. " if ordered else "- "
            sub = self.blocks(li, {**ctx, "in_list": True})
            if not sub:
                continue
            pad = " " * len(marker)
            if lines and loose:
                lines.append("")
            first = True
            for b in sub:
                for j, line in enumerate(b.split("\n")):
                    if first and j == 0:
                        lines.append(marker + line)
                    elif j == 0:
                        lines.append("")
                        lines.append(pad + line if line else "")
                    else:
                        lines.append(pad + line if line else "")
                first = False
        return "\n".join(lines)

    def table(self, n):
        rows = []
        head = None
        for tr in n.find_all(lambda x: x.tag == "tr"):
            cells = [c for c in tr.children if isinstance(c, Node) and c.tag in ("td", "th")]
            vals = [self.inline(c.children).strip().replace("|", "\\|") or " " for c in cells]
            in_head = tr.parent is not None and tr.parent.tag == "thead"
            if in_head and head is None:
                head = vals
            else:
                rows.append(vals)
        if head is None and rows:
            head = rows.pop(0)
        out = ["| " + " | ".join(head) + " |", "|" + "|".join(["---"] * len(head)) + "|"]
        out += ["| " + " | ".join(r) + " |" for r in rows]
        return "\n".join(out)


def convert(path, base, title=None, shift=0, lead=None):
    b = Builder()
    with open(path, encoding="utf-8") as f:
        b.feed(f.read())
    main = next(b.root.find_all(lambda x: x.tag == "main"))
    r = Renderer(base, shift)
    blocks = r.blocks(main)
    if title:
        # replace the first H1 with the override
        for i, blk in enumerate(blocks):
            if blk.startswith("# ") or (shift and blk.startswith("#" * (1 + shift) + " ")):
                blocks[i] = r.heading(1, title)
                break
    if lead:
        blocks.insert(1, lead)
    return tidy("\n\n".join(blocks))


def tidy(md):
    md = re.sub(r"[ \t]+\n", "\n", md)
    md = re.sub(r"\n{3,}", "\n\n", md)
    return md.strip() + "\n"


def convert_sections(path, base, sections, shift=0):
    """Render only the <section> elements with the given ids, in the given order.

    sections: list of (id, heading) — heading replaces the section's first heading."""
    b = Builder()
    with open(path, encoding="utf-8") as f:
        b.feed(f.read())
    out = []
    for sid, heading in sections:
        node = next(b.root.find_all(lambda x: x.attrs.get("id") == sid))
        r = Renderer(base, shift)
        blocks = r.block(node, {})
        if heading:
            for i, blk in enumerate(blocks):
                if blk.startswith("#"):
                    level = len(blk) - len(blk.lstrip("#"))
                    blocks[i] = "#" * level + " " + heading
                    break
        out.append("\n\n".join(blocks))
    return tidy("\n\n".join(out))


if __name__ == "__main__":
    args = sys.argv[1:]
    title = shift = lead = None
    if "--title" in args:
        i = args.index("--title")
        title = args[i + 1]
        del args[i:i + 2]
    if "--lead" in args:
        i = args.index("--lead")
        lead = args[i + 1]
        del args[i:i + 2]
    if "--shift" in args:
        i = args.index("--shift")
        shift = int(args[i + 1])
        del args[i:i + 2]
    sys.stdout.write(convert(args[0], args[1], title, shift or 0, lead))
