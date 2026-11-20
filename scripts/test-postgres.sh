#!/bin/sh
# Run the Go checks against a throwaway Postgres server: initdb and pg_ctl
# from PATH (or PG_BIN), a temporary data directory and Unix socket, removed
# on exit. Extra arguments go to `go test` (default ./...).
set -eu
bin=${PG_BIN:-}
initdb=${bin:+$bin/}initdb
pg_ctl=${bin:+$bin/}pg_ctl
dir=$(mktemp -d)
port=${PG_PORT:-54329}
cleanup() { "$pg_ctl" -D "$dir/data" -m immediate stop >/dev/null 2>&1 || true; rm -rf "$dir"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# UTF8 as the shared Postgres: jsonb refuses \u escapes above 007F otherwise.
"$initdb" -D "$dir/data" -U postgres -A trust -E UTF8 --no-locale >/dev/null
"$pg_ctl" -D "$dir/data" -o "-p $port -k $dir -c listen_addresses=''" -l "$dir/log" -w start >/dev/null
export GORDON_TEST_DATABASE_URL="postgres://postgres@/postgres?host=$dir&port=$port&sslmode=disable"
go build ./...
go vet ./...
if [ $# -eq 0 ]; then set -- ./...; fi
go test "$@"
