#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
# Copyright (C) 2026 Cenvero / Shubhdeep Singh
"""Dismiss CodeQL alerts that the code marks as reviewed false positives.

A finding that has been reviewed and is not a vulnerability is marked in the
source, on the flagged line or the line directly above it, with the rule id and
the reason:

    // codeql[go/path-injection] operator-chosen local path; see cleanLocal

This is the same `codeql[<rule>]` comment form CodeQL's own alert-suppression
query recognises. After CodeQL has uploaded its results for the default branch,
this script lists the open CodeQL alerts and dismisses — as "false positive",
with the marker's reason as the comment — only those whose location carries a
marker naming that alert's rule. Every other alert stays open, and a marker
covers only the rule(s) it names, so a new, different finding on a marked line
is still reported. Markers are reviewed like any other code change.

Usage (in CI, with GITHUB_TOKEN, GITHUB_REPOSITORY and GITHUB_REF set):
    python3 .github/scripts/codeql_dismiss_marked.py
Locally, against a SARIF file, to see what would stay open (exit 1 if any):
    python3 .github/scripts/codeql_dismiss_marked.py --sarif results.sarif
"""
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

MARKER = re.compile(r"codeql\[([A-Za-z0-9_/,\- ]+)\]\s*(?:--|:)?\s*(.*)")
DEFAULT_REASON = "Reviewed false positive; marked in the code."


def marker_reason(path, line, rule):
    """Return the reason text if `line` (1-based) of `path` or the line above
    it marks `rule`, else None. Paths outside the checkout are never read."""
    if not path or os.path.isabs(path) or ".." in path.replace("\\", "/").split("/"):
        return None
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            lines = f.read().split("\n")
    except OSError:
        return None
    for n in (line, line - 1):
        if 1 <= n <= len(lines):
            m = MARKER.search(lines[n - 1])
            if m and rule in [r.strip() for r in m.group(1).split(",")]:
                reason = m.group(2).strip().rstrip("*/").strip()
                return (reason or DEFAULT_REASON)[:280]
    return None


def check_sarif(path):
    with open(path, encoding="utf-8") as f:
        sarif = json.load(f)
    open_left = 0
    for run in sarif.get("runs", []):
        for result in run.get("results", []):
            loc = result["locations"][0]["physicalLocation"]
            file_path = loc["artifactLocation"]["uri"]
            line = loc["region"]["startLine"]
            rule = result["ruleId"]
            reason = marker_reason(file_path, line, rule)
            if reason is None:
                open_left += 1
                print(f"OPEN      {rule} {file_path}:{line}")
            else:
                print(f"DISMISSED {rule} {file_path}:{line} -- {reason}")
    print(f"{open_left} result(s) would stay open")
    return 1 if open_left else 0


def api(method, url, token, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers={
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "cenvero-fleet-codeql-dismiss",
    })
    with urllib.request.urlopen(req, timeout=30) as resp:  # nosec B310 -- fixed https API URL
        return json.loads(resp.read() or b"null")


def dismiss_in_github():
    token = os.environ["GITHUB_TOKEN"]
    repo = os.environ["GITHUB_REPOSITORY"]
    ref = os.environ.get("GITHUB_REF", "refs/heads/main")
    base = os.environ.get("GITHUB_API_URL", "https://api.github.com").rstrip("/")
    if not base.startswith("https://"):
        raise SystemExit("GITHUB_API_URL must be https")
    # List every open alert first: dismissing while paging through
    # state=open would shift later pages and skip alerts.
    alerts = []
    page = 1
    while True:
        query = urllib.parse.urlencode({"state": "open", "tool_name": "CodeQL", "ref": ref, "per_page": 100, "page": page})
        batch = api("GET", f"{base}/repos/{repo}/code-scanning/alerts?{query}", token)
        if not batch:
            break
        alerts.extend(batch)
        page += 1
    dismissed = kept = 0
    for alert in alerts:
        loc = (alert.get("most_recent_instance") or {}).get("location") or {}
        rule = (alert.get("rule") or {}).get("id", "")
        reason = marker_reason(loc.get("path", ""), int(loc.get("start_line") or 0), rule)
        where = f"#{alert['number']} {rule} {loc.get('path')}:{loc.get('start_line')}"
        if reason is None:
            kept += 1
            print(f"open      {where}")
            continue
        api("PATCH", f"{base}/repos/{repo}/code-scanning/alerts/{alert['number']}", token,
            {"state": "dismissed", "dismissed_reason": "false positive", "dismissed_comment": reason})
        dismissed += 1
        print(f"dismissed {where} -- {reason}")
    print(f"dismissed {dismissed} marked alert(s); {kept} alert(s) left open")


def main(argv):
    if len(argv) == 3 and argv[1] == "--sarif":
        return check_sarif(argv[2])
    if len(argv) != 1:
        print(__doc__, file=sys.stderr)
        return 2
    try:
        dismiss_in_github()
    except urllib.error.HTTPError as e:
        print(f"GitHub API error {e.code}: {e.read()[:500]!r}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
