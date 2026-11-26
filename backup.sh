#!/bin/sh
set -eu
cd "$(dirname "$0")"

db=${1:-data/status.db}
dest_dir=${BACKUP_DIR:-data/backups}
[ -f "$db" ] || { printf 'database not found: %s\n' "$db" >&2; exit 1; }

mkdir -p "$dest_dir"
chmod 700 "$dest_dir"
out="$dest_dir/$(basename "$db" .db)-$(date +%Y%m%d-%H%M%S).sqlite"

python3 - "$db" "$out" <<'PY'
import os, sqlite3, sys
src, dst = sys.argv[1], sys.argv[2]
os.umask(0o077)
with sqlite3.connect(src) as source, sqlite3.connect(dst) as target:
    source.backup(target)
    target.execute('PRAGMA journal_mode=DELETE')
    result = target.execute('PRAGMA integrity_check').fetchone()[0]
if result != 'ok':
    raise SystemExit(f'integrity check failed: {result}')
PY

chmod 600 "$out"
printf 'backup: %s (%s bytes)\n' "$out" "$(wc -c < "$out")"
