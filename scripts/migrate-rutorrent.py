#!/usr/bin/env python3
"""migrate-rutorrent.py — one-shot tool to convert rutorrent RSS state to watchlist.yaml.

usage: scripts/migrate-rutorrent.py <private-dir>

reads <private-dir>/info and <private-dir>/filters, emits watchlist.yaml on stdout.
the output file contains tracker passkeys — do not commit it.
"""

import re
import sys
from pathlib import Path


# ---------------------------------------------------------------------------
# php-serialize parser (hand-rolled, no pip deps)
# ---------------------------------------------------------------------------

class PhpParser:
    def __init__(self, data: str):
        self.data = data
        self.pos = 0

    def expect(self, ch: str) -> None:
        if self.data[self.pos] != ch:
            raise ValueError(f"expected {ch!r} at pos {self.pos}, got {self.data[self.pos]!r}")
        self.pos += 1

    def read_int_until(self, terminator: str) -> str:
        start = self.pos
        idx = self.data.index(terminator, self.pos)
        self.pos = idx + len(terminator)
        return self.data[start:idx]

    def parse_value(self):
        t = self.data[self.pos]
        self.pos += 1
        self.expect(":")

        if t == "i":
            v = int(self.read_int_until(";"))
            return v

        if t == "b":
            v = self.read_int_until(";")
            return v == "1"

        if t == "s":
            length = int(self.read_int_until(":"))
            self.expect('"')
            v = self.data[self.pos : self.pos + length]
            self.pos += length
            self.expect('"')
            self.expect(";")
            return v

        if t == "a":
            count = int(self.read_int_until(":"))
            self.expect("{")
            result = {}
            for _ in range(count):
                k = self.parse_value()
                v = self.parse_value()
                result[k] = v
            self.expect("}")
            return result

        if t == "O":
            name_len = int(self.read_int_until(":"))
            self.expect('"')
            _class_name = self.data[self.pos : self.pos + name_len]
            self.pos += name_len
            self.expect('"')
            self.expect(":")
            prop_count = int(self.read_int_until(":"))
            self.expect("{")
            result = {}
            for _ in range(prop_count):
                k = self.parse_value()
                v = self.parse_value()
                result[k] = v
            self.expect("}")
            return result

        if t == "N":
            self.expect(";")
            return None

        raise ValueError(f"unknown type {t!r} at pos {self.pos - 2}")


def parse_php(data: str):
    parser = PhpParser(data)
    return parser.parse_value()


# ---------------------------------------------------------------------------
# pattern translation: /body/flags  ->  (?flags)body
# ---------------------------------------------------------------------------

def translate_pattern(raw: str) -> str:
    """convert rutorrent PCRE pattern to go regexp syntax."""
    if not raw:
        return raw

    # check for surrounding slashes: /pattern/flags
    m = re.match(r'^/(.*)/([\w]*)$', raw, re.DOTALL)
    if not m:
        # bare regex — emit as-is
        return raw

    body = m.group(1)
    flags = m.group(2)

    if flags:
        return f"(?{flags}){body}"
    return body


# ---------------------------------------------------------------------------
# yaml helpers (hand-rolled to avoid pyyaml dep)
# ---------------------------------------------------------------------------

def yaml_str(s: str) -> str:
    """quote a string for yaml if it contains special chars."""
    if not s:
        return "''"
    # use single quotes if the string contains chars that need quoting
    if re.search(r'[:#\[\]{}&*!,|>\'"\\]', s) or s.startswith(' ') or s.endswith(' '):
        escaped = s.replace("'", "''")
        return f"'{escaped}'"
    return s


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

def main():
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} <private-dir>", file=sys.stderr)
        sys.exit(1)

    private = Path(sys.argv[1])
    info_path = private / "info"
    filters_path = private / "filters"

    for p in (info_path, filters_path):
        if not p.exists():
            print(f"error: {p} not found", file=sys.stderr)
            sys.exit(1)

    info_obj = parse_php(info_path.read_text(encoding="utf-8"))
    filters_obj = parse_php(filters_path.read_text(encoding="utf-8"))

    # ---- extract feeds -------------------------------------------------------
    feeds_raw = info_obj.get("lst", {})
    feeds = []
    # rssHash → feed label, so filters can be scoped back to their feed
    hash_to_label = {}
    for rss_hash, entry in feeds_raw.items():
        url = entry.get("url", "")
        label = entry.get("label", "")
        enabled = entry.get("enabled")
        # enabled is int in info file
        is_enabled = (enabled == 1 or enabled == "1" or enabled is True)
        feeds.append({"url": url, "label": label, "enabled": is_enabled})
        if label:
            hash_to_label[rss_hash] = label

    enabled_feeds = sorted(
        [f for f in feeds if f["enabled"]], key=lambda f: f["url"]
    )
    disabled_feeds = sorted(
        [f for f in feeds if not f["enabled"]], key=lambda f: f["url"]
    )

    # ---- extract filters / shows ---------------------------------------------
    filters_raw = filters_obj.get("lst", {})
    shows = []
    for entry in filters_raw.values():
        name = entry.get("name", "")
        pattern = entry.get("pattern", "")
        exclude = entry.get("exclude", "")
        enabled_val = entry.get("enabled")
        is_enabled = not (
            enabled_val == "0" or enabled_val == 0 or enabled_val is False
        )
        rss_hash = entry.get("rssHash", "")
        feed_label = hash_to_label.get(rss_hash, "")
        shows.append({
            "name": name.strip(),
            "pattern": pattern,
            "exclude": exclude,
            "enabled": is_enabled,
            "feed": feed_label,
        })

    shows_sorted = sorted(shows, key=lambda s: s["name"].lower())

    # ---- emit yaml -----------------------------------------------------------
    lines = [
        "# auto-generated by scripts/migrate-rutorrent.py",
        "# review and merge into deploy/k8s/configmap.yaml under data.watchlist.yaml",
        "",
        "seedctl_url: REPLACE_ME    # e.g. https://tiny.example.com:18443",
        "",
        "shows:",
    ]

    for show in shows_sorted:
        translated = translate_pattern(show["pattern"])
        name_q = yaml_str(show["name"])
        pat_q = yaml_str(translated)

        feed_list = f"[{yaml_str(show['feed'])}]" if show["feed"] else ""

        if show["enabled"]:
            lines.append(f"  - name: {name_q}")
            lines.append(f"    regex: {pat_q}")
            if feed_list:
                lines.append(f"    feeds: {feed_list}")
            if show["exclude"]:
                excl_q = yaml_str(show["exclude"])
                lines.append(f"    # exclude in rutorrent (NOT applied): {excl_q}")
        else:
            # disabled: emit as comment block
            lines.append(f"  # - name: {name_q}  # disabled in rutorrent")
            lines.append(f"  #   regex: {pat_q}")
            if feed_list:
                lines.append(f"  #   feeds: {feed_list}")
            if show["exclude"]:
                excl_q = yaml_str(show["exclude"])
                lines.append(f"  #   # exclude in rutorrent (NOT applied): {excl_q}")

    lines.append("")
    lines.append("feeds:")

    for feed in enabled_feeds:
        label = feed["label"]
        url = feed["url"]
        if label:
            lines.append(f"  - name: {yaml_str(label)}")
            lines.append(f"    url: {url}")
        else:
            lines.append(f"  - url: {url}")
        lines.append(f"    # cookie_env: TRACKER_X_COOKIE   # uncomment + set if this tracker needs a cookie")

    for feed in disabled_feeds:
        label = feed["label"]
        url = feed["url"]
        comment = f"  # {label} (disabled)" if label else "  # disabled"
        lines.append(f"  # - url: {url}{comment}")
        lines.append(f"  #   # cookie_env: TRACKER_X_COOKIE")

    print("\n".join(lines))


if __name__ == "__main__":
    main()
