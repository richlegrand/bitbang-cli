#!/bin/bash
# Print or set the version in cmd/bitbang/main.go
# Usage:
#   ./version.sh          # prints current version
#   ./version.sh 0.2.0    # sets version to 0.2.0

FILE="$(dirname "$0")/cmd/bitbang/main.go"

if [ -z "$1" ]; then
    grep 'const version' "$FILE" | sed 's/.*"\(.*\)".*/\1/'
else
    sed -i "s/const version = \".*\"/const version = \"$1\"/" "$FILE"
    echo "$1"
fi
