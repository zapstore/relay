#!/usr/bin/env bash
# Adds the columns that older relay databases lack, with the relay stopped. The
# relay no longer does this at startup: it refuses a database that has not been
# through this script. Run it once, as the user that owns the databases, before
# starting the new relay:
#
#   systemctl stop relay
#   scripts/add-columns.sh /var/lib/zapstore/data
#   (deploy the new binary)
#   systemctl start relay
#
# It touches two databases in the data directory, each only if it needs to:
#
#   relay.db    pending_events.app_id, filled from each stored event's tags (d on
#               an app, i on a release or asset) and indexed;
#               ban_notices.kind and ban_notices.id, empty on a notice written
#               before them, which only means lifting that ban will not drop it.
#   blossom.db  blobs.state, 'served' for every blob it holds, and its index.
#
# Before it changes a database it writes a full copy next to it
# (<name>.before-add-columns-<stamp>) and checks the copy. Nothing is deleted.
# Re-running is safe: a database that has the columns is left alone, and when
# neither needs anything the script says so and exits.
#
# This is separate from drop-slots.sh, which retires the two-slot design in
# relay.db. They touch different things and can run in either order.
#
# Usage: add-columns.sh [-y] <data-dir>
#   data-dir  holds relay.db and blossom.db (DATA_DIR, /var/lib/zapstore/data)
set -euo pipefail

yes=0
if [ "${1:-}" = "-y" ]; then
	yes=1
	shift
fi
if [ $# -ne 1 ]; then
	echo "usage: $0 [-y] <data-dir>" >&2
	exit 2
fi
dir=${1%/}
relay=$dir/relay.db
blossom=$dir/blossom.db

die() { echo "$*" >&2; exit 1; }

command -v sqlite3 >/dev/null || die "sqlite3 is not installed"
[ -f "$relay" ] || die "no such database: $relay"
if command -v systemctl >/dev/null && systemctl is-active --quiet relay 2>/dev/null; then
	die "the relay is running; stop it first: systemctl stop relay"
fi
for db in "$relay" "$blossom"; do
	[ -f "$db" ] || continue
	if command -v lsof >/dev/null && lsof "$db" >/dev/null 2>&1; then
		die "$db is open by a running process; stop the relay first"
	fi
	if [ -s "$db-wal" ]; then
		die "$db-wal is not empty: the relay did not stop cleanly. Start and stop it once, then retry"
	fi
done

# lacks DB TABLE COLUMN: succeeds when the table exists and has no such column.
lacks() {
	local n
	n=$(sqlite3 "$1" "SELECT COUNT(*) || ',' || COALESCE(SUM(name = '$3'), 0) FROM pragma_table_info('$2')")
	[ "${n%,*}" -gt 0 ] && [ "${n#*,}" -eq 0 ]
}

pending=0 notices=0 blobs=0
lacks "$relay" pending_events app_id && pending=1
lacks "$relay" ban_notices kind && notices=1
if [ -f "$blossom" ] && lacks "$blossom" blobs state; then blobs=1; fi

if [ $((pending + notices + blobs)) -eq 0 ]; then
	echo "nothing to do: the databases in $dir already have these columns"
	exit 0
fi

echo "in $dir:"
if [ "$pending" -eq 1 ]; then
	echo "  relay.db: pending_events.app_id, filled for $(sqlite3 "$relay" "SELECT COUNT(*) FROM pending_events WHERE kind IN (32267, 30063, 3063)") of $(sqlite3 "$relay" "SELECT COUNT(*) FROM pending_events") pending events, and indexed"
fi
if [ "$notices" -eq 1 ]; then
	echo "  relay.db: ban_notices.kind and id, empty for $(sqlite3 "$relay" "SELECT COUNT(*) FROM ban_notices") existing notices"
fi
if [ "$blobs" -eq 1 ]; then
	echo "  blossom.db: blobs.state, 'served' for $(sqlite3 "$blossom" "SELECT COUNT(*) FROM blobs") blobs, and indexed"
fi

if [ "$yes" -ne 1 ]; then
	read -r -p "Back up and apply? [y/N] " answer
	[ "$answer" = "y" ] || { echo "nothing changed"; exit 1; }
fi

stamp=$(date +%Y%m%d-%H%M%S)
backup() {
	local copy="$1.before-add-columns-$stamp"
	sqlite3 "$1" "VACUUM INTO '$copy'"
	sqlite3 "$copy" "PRAGMA integrity_check;" | grep -qx ok || die "the backup of $1 failed its integrity check"
	echo "backup: $copy"
}

if [ $((pending + notices)) -gt 0 ]; then
	backup "$relay"
	fk_before=$(sqlite3 "$relay" "PRAGMA foreign_key_check;" | wc -l | tr -d ' ')
	{
		echo ".bail on"
		echo "BEGIN IMMEDIATE;"
		if [ "$pending" -eq 1 ]; then
			cat <<'SQL'
ALTER TABLE pending_events ADD COLUMN app_id TEXT NOT NULL DEFAULT '';
-- The first d on an app (32267), the first i on a release (30063) or asset (3063).
UPDATE pending_events SET app_id = coalesce((
    SELECT json_extract(value, '$[1]') FROM json_each(json_extract(raw, '$.tags'))
    WHERE json_extract(value, '$[0]') = CASE kind WHEN 32267 THEN 'd' ELSE 'i' END
    LIMIT 1
), '')
WHERE kind IN (32267, 30063, 3063);
CREATE INDEX IF NOT EXISTS idx_pending_events_app_id ON pending_events(app_id);
SQL
		fi
		if [ "$notices" -eq 1 ]; then
			cat <<'SQL'
ALTER TABLE ban_notices ADD COLUMN kind TEXT NOT NULL DEFAULT '';
ALTER TABLE ban_notices ADD COLUMN id TEXT NOT NULL DEFAULT '';
SQL
		fi
		echo "COMMIT;"
	} | sqlite3 "$relay"

	sqlite3 "$relay" "PRAGMA integrity_check;" | grep -qx ok || die "$relay failed its integrity check; restore the backup"
	fk_after=$(sqlite3 "$relay" "PRAGMA foreign_key_check;" | wc -l | tr -d ' ')
	[ "$fk_after" -le "$fk_before" ] || die "$relay has new foreign key violations; restore the backup"
	if [ "$pending" -eq 1 ] && lacks "$relay" pending_events app_id; then die "pending_events.app_id was not added; restore the backup"; fi
	if [ "$notices" -eq 1 ] && lacks "$relay" ban_notices kind; then die "ban_notices.kind was not added; restore the backup"; fi
	echo "relay.db done"
fi

if [ "$blobs" -eq 1 ]; then
	backup "$blossom"
	sqlite3 "$blossom" <<'SQL'
.bail on
BEGIN IMMEDIATE;
ALTER TABLE blobs ADD COLUMN state TEXT NOT NULL DEFAULT 'served';
CREATE INDEX IF NOT EXISTS idx_blobs_state ON blobs(state);
COMMIT;
SQL
	sqlite3 "$blossom" "PRAGMA integrity_check;" | grep -qx ok || die "$blossom failed its integrity check; restore the backup"
	if lacks "$blossom" blobs state; then die "blobs.state was not added; restore the backup"; fi
	echo "blossom.db done"
fi

echo "done. Deploy the new relay and start it."
