#!/usr/bin/env python3
"""Validate a PR body against the repository template without project dependencies."""

import argparse
import json
import os
from pathlib import Path
import re
import sys


def validate(template, body):
    headings = re.findall(r"^#{4,6}\s+.+$", template, re.MULTILINE)
    if not headings:
        return ["No markdown headings found in PR template."]
    errors = []
    positions = [body.find(heading) for heading in headings]
    for heading, position in zip(headings, positions):
        if position < 0:
            errors.append(f"Missing required heading: {heading}")
    present = [position for position in positions if position >= 0]
    if present != sorted(present):
        errors.append("Required headings are out of order.")
    if "<!--" in body:
        errors.append("PR description still contains template placeholder comments.")

    def section(document, heading):
        start = document.find(heading)
        if start < 0:
            return ""
        start += len(heading)
        if document[start:start + 2] != "\n\n":
            return ""
        start += 2
        ends = [document.find("\n" + marker, start) for marker in headings if marker != heading]
        end = min((position for position in ends if position >= 0), default=len(document))
        return document[start:end]

    for heading, position in zip(headings, positions):
        if position < 0:
            continue
        expected, actual = section(template, heading), section(body, heading)
        if not actual.strip():
            errors.append(f"Section cannot be empty: {heading}")
        elif re.search(r"^- ", expected, re.MULTILINE) and not re.search(r"^- ", actual, re.MULTILINE):
            errors.append(f"Section must include at least one bullet item: {heading}")
        if re.search(r"^- \[ \] ", expected, re.MULTILINE) and not re.search(r"^- \[[ xX]\] ", actual, re.MULTILINE):
            errors.append(f"Section must include at least one checkbox item: {heading}")
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--file", type=Path, help="Markdown PR body; otherwise use PR_BODY_JSON")
    args = parser.parse_args()
    template_path = Path(__file__).resolve().parents[1] / "pull_request_template.md"
    try:
        template = template_path.read_text()
        body = args.file.read_text() if args.file else json.loads(os.environ["PR_BODY_JSON"])
        if not isinstance(body, str):
            raise ValueError("PR body must be text")
    except (OSError, KeyError, ValueError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    errors = validate(template, body)
    for error in errors:
        print(f"ERROR: {error}", file=sys.stderr)
    if errors:
        return 1
    print("PR body format OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
