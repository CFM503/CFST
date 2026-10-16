#!/usr/bin/env python3
# -*- coding: utf-8 -*-

import os
import re
import sys

def main():
    tag = os.environ.get("TAG_NAME", "").strip()
    if not tag and len(sys.argv) > 1:
        tag = sys.argv[1].strip()

    notes = f"CFST {tag} Release\n"
    if os.path.exists("CHANGELOG.md") and tag:
        try:
            with open("CHANGELOG.md", "r", encoding="utf-8") as f:
                content = f.read()
            pattern = rf"(?s)## {re.escape(tag)}.*?(?=\n## |\Z)"
            m = re.search(pattern, content)
            if m:
                notes = m.group(0).strip() + "\n"
        except Exception as e:
            print(f"Warning: failed to parse CHANGELOG.md: {e}", file=sys.stderr)

    with open("RELEASE_NOTES.md", "w", encoding="utf-8") as f:
        f.write(notes)
    print(f"Successfully generated RELEASE_NOTES.md for {tag}")

if __name__ == "__main__":
    main()
