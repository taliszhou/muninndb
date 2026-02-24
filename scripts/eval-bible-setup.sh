#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATA_DIR="$SCRIPT_DIR/../testdata/bible"

mkdir -p "$DATA_DIR"

KJV_URL="https://raw.githubusercontent.com/scrollmapper/bible_databases/master/json/t_kjv.json"
KJV_FILE="$DATA_DIR/kjv.json"
if [ ! -f "$KJV_FILE" ]; then
    echo "Downloading KJV Bible JSON..."
    curl -fsSL "$KJV_URL" -o "$KJV_FILE"
    echo "  → saved to $KJV_FILE ($(du -h "$KJV_FILE" | cut -f1))"
else
    echo "KJV Bible JSON already present: $KJV_FILE"
fi

XREF_URL="https://a.openbible.info/refs/cross-references.tsv"
XREF_FILE="$DATA_DIR/cross-refs.tsv"
if [ ! -f "$XREF_FILE" ]; then
    echo "Downloading OpenBible cross-references..."
    curl -fsSL "$XREF_URL" -o "$XREF_FILE"
    echo "  → saved to $XREF_FILE ($(du -h "$XREF_FILE" | cut -f1))"
else
    echo "Cross-references already present: $XREF_FILE"
fi

echo ""
echo "Setup complete."
ls -lh "$DATA_DIR"/*.json "$DATA_DIR"/*.tsv 2>/dev/null || true
