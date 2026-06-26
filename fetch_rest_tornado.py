#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "requests>=2.31",
#     "beautifulsoup4>=4.12",
#     "markdownify>=0.13",
# ]
# ///
"""Fetch the salt-api ``rest_tornado`` reference page and emit clean markdown.

The raw HTML is cached to disk on first run so subsequent runs (while we tweak
the extraction/conversion steps) don't re-hit the network. Delete the cache
file or pass --refresh to force a fresh fetch.

Run with uv (auto-installs deps into an ephemeral env):

    uv run fetch_rest_tornado.py

Output: docs/salt-api-rest_tornado.md
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

import requests
from bs4 import BeautifulSoup, Tag
from markdownify import MarkdownConverter

HEADING_TAGS = ("h1", "h2", "h3", "h4", "h5", "h6")

URL = "https://docs.saltproject.io/en/master/ref/netapi/all/salt.netapi.rest_tornado.html"

HERE = Path(__file__).resolve().parent
CACHE_FILE = HERE / ".cache" / "rest_tornado.html"
OUTPUT_FILE = HERE / "docs" / "salt-api-rest_tornado.md"

# Elements inside the article that are chrome, not content.
JUNK_SELECTORS = [
    "div.bd-header-article",          # in-page breadcrumb/edit header
    "div#ethical-ad-placement",       # ads
    "div.ethical-ad-placement",
    "footer.prev-next-footer",        # prev/next nav
    "div.prev-next-area",
    "a.headerlink",                   # the ¶ anchors next to headings
]


def fetch_html(refresh: bool = False) -> str:
    """Return the page HTML, using the on-disk cache when available."""
    if CACHE_FILE.exists() and not refresh:
        return CACHE_FILE.read_text(encoding="utf-8")

    resp = requests.get(URL, timeout=30, headers={"User-Agent": "papurika-docs-fetcher"})
    resp.raise_for_status()
    html = resp.text

    CACHE_FILE.parent.mkdir(parents=True, exist_ok=True)
    CACHE_FILE.write_text(html, encoding="utf-8")
    return html


def extract_article(html: str) -> Tag:
    """Pull out the main article node and strip non-content chrome."""
    soup = BeautifulSoup(html, "html.parser")

    article = soup.select_one("article.bd-article") or soup.select_one("main#main-content")
    if article is None:
        raise SystemExit("Could not locate the main article node in the HTML.")

    for selector in JUNK_SELECTORS:
        for node in article.select(selector):
            node.decompose()

    return article


# Markers we inject into the DOM as placeholder text, then swap for real HTML
# anchors after markdownify has run. (markdownify drops empty <a> tags, so we
# can't inject the anchors directly.) The {i} indexes ANCHOR_REGISTRY.
BLOCK_MARKER = "zzANCHORBLOCK{i}zz"
INLINE_MARKER = "zzANCHORINLINE{i}zz"
ANCHOR_REGISTRY: list[str] = []

_BLOCK_RE = re.compile(r"[ \t]*zzANCHORBLOCK(\d+)zz[ \t]*")
_INLINE_RE = re.compile(r"zzANCHORINLINE(\d+)zz")


def inject_anchors(article: Tag) -> None:
    """Plant placeholder markers at every ``id`` target in the article.

    Sphinx gives elements fragment ids (``RunSaltAPIHandler``, ``rest-tornado-auth``,
    ``id4`` …) that the page's internal links point at. Markdown loses those ids,
    so we record each one and drop a marker that ``to_markdown`` later turns into
    an ``<a id="…"></a>`` jump target. Headings need the anchor on their own line
    above them; inline definitions take a zero-width anchor in place.
    """
    ANCHOR_REGISTRY.clear()
    for el in article.find_all(id=True):
        anchor_id = el.get("id")
        if not anchor_id:
            continue
        index = len(ANCHOR_REGISTRY)
        ANCHOR_REGISTRY.append(anchor_id)

        if el.name == "dt":
            # Zero-width anchor at the start of the definition term line.
            el.insert(0, INLINE_MARKER.format(i=index))
        elif el.name == "a":
            # Backref target living inside a heading: anchor goes above it.
            heading = el.find_parent(HEADING_TAGS)
            (heading or el).insert_before(BLOCK_MARKER.format(i=index))
        else:
            # section / nav / span: own line before the element's content.
            el.insert_before(BLOCK_MARKER.format(i=index))


def _resolve_anchors(md: str) -> str:
    md = _INLINE_RE.sub(
        lambda m: f'<a id="{ANCHOR_REGISTRY[int(m.group(1))]}"></a>', md
    )
    md = _BLOCK_RE.sub(
        lambda m: f'\n\n<a id="{ANCHOR_REGISTRY[int(m.group(1))]}"></a>\n\n', md
    )
    return md


class SaltMarkdownConverter(MarkdownConverter):
    """MarkdownConverter that recovers fenced-code languages from Sphinx markup.

    Sphinx (PyData theme) wraps code as::

        <div class="highlight-bash notranslate"><div class="highlight"><pre>...

    The language lives in the ``highlight-<lang>`` class on an ancestor div, so
    we map ``<pre>`` -> fenced block with the right language hint.
    """

    LANG_PREFIX = "highlight-"

    def _lang_for(self, pre: Tag) -> str:
        node = pre.parent
        while node is not None and isinstance(node, Tag):
            for cls in node.get("class", []) or []:
                if cls.startswith(self.LANG_PREFIX):
                    lang = cls[len(self.LANG_PREFIX):]
                    return "" if lang in ("default", "text") else lang
            node = node.parent
        return ""

    def convert_pre(self, el, text, parent_tags):
        if not text:
            return ""
        lang = self._lang_for(el)
        code = el.get_text()
        code = code.rstrip("\n")
        return f"\n\n```{lang}\n{code}\n```\n\n"


def to_markdown(article: Tag) -> str:
    converter = SaltMarkdownConverter(
        heading_style="ATX",
        bullets="-",
        code_language_callback=None,
        escape_asterisks=False,
        escape_underscores=False,
    )
    md = converter.convert_soup(article)
    md = _resolve_anchors(md)

    # Collapse runs of 3+ blank lines down to a single blank line.
    lines = md.splitlines()
    cleaned: list[str] = []
    blanks = 0
    for line in lines:
        stripped = line.rstrip()
        if not stripped:
            blanks += 1
            if blanks > 1:
                continue
        else:
            blanks = 0
        cleaned.append(stripped)

    body = "\n".join(cleaned).strip()

    header = (
        "<!-- Generated by fetch_rest_tornado.py — do not edit by hand. -->\n"
        f"<!-- Source: {URL} -->\n\n"
    )
    return header + body + "\n"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--refresh", action="store_true", help="Ignore the cache and re-fetch the page."
    )
    parser.add_argument(
        "-o", "--output", type=Path, default=OUTPUT_FILE, help="Output markdown path."
    )
    args = parser.parse_args(argv)

    html = fetch_html(refresh=args.refresh)
    article = extract_article(html)
    inject_anchors(article)
    markdown = to_markdown(article)

    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(markdown, encoding="utf-8")

    print(f"Wrote {len(markdown):,} bytes to {args.output}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
