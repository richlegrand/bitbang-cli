#!/bin/bash
# Run BitBangProxy tests
#
# Usage:
#   ./run_tests.sh              # run all tests (unit + e2e)
#   ./run_tests.sh unit         # run Go unit tests only
#   ./run_tests.sh e2e          # run E2E tests only
#   ./run_tests.sh e2e test_proxy_page  # run a specific E2E test

set -e

cd "$(dirname "$0")"

export PATH=/usr/local/go/bin:$PATH
export BITBANG_TEST_SERVER="${BITBANG_TEST_SERVER:-test.bitba.ng}"

case "${1:-all}" in
    unit)
        echo "Running unit tests..."
        # ./... rather than ./internal/...; the latter silently skips
        # every test under cmd/bitbang (arg reordering, serve-mode
        # routing, the device table, share state), which is what CI runs.
        go test ./... -v
        ;;
    e2e)
        echo "Running E2E tests..."
        mkdir -p tests/e2e/screenshots
        if [ -n "$2" ]; then
            python3 -m pytest "tests/e2e/test_${2}.py" -v --screenshot=only-on-failure --output=tests/e2e/screenshots "${@:3}"
        else
            python3 -m pytest tests/e2e/ -v --screenshot=only-on-failure --output=tests/e2e/screenshots
        fi
        ;;
    all)
        echo "Running unit tests..."
        # ./... rather than ./internal/...; the latter silently skips
        # every test under cmd/bitbang (arg reordering, serve-mode
        # routing, the device table, share state), which is what CI runs.
        go test ./... -v
        echo ""
        echo "Running E2E tests..."
        mkdir -p tests/e2e/screenshots
        python3 -m pytest tests/e2e/ -v --screenshot=only-on-failure --output=tests/e2e/screenshots
        ;;
esac
