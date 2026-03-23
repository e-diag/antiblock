#!/usr/bin/env bash
set -e
echo "=== AntiBlock Bot — Test Mode ==="

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [ ! -f .env.test ]; then
    echo "ERROR: .env.test not found"
    exit 1
fi

if ! psql -lqt 2>/dev/null | cut -d \| -f 1 | grep -qw antiblock_test; then
    echo "Creating test database..."
    createdb antiblock_test
fi

set -a
# shellcheck disable=SC1090
source <(grep -v '^#' .env.test | grep -v '^$' | sed 's/^/export /')
set +a

echo "Run bot once to apply migrations, then:"
echo "  psql -d antiblock_test -f scripts/setup_test_db.sql"
echo "Starting bot..."
exec go run ./cmd/bot
