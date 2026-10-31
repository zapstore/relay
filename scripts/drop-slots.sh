#!/usr/bin/env bash
# Retires the two-slot design in relay.db. Run it once, with the relay stopped,
# before starting the release-chain relay:
#
#   systemctl stop relay
#   scripts/drop-slots.sh /var/lib/relay/relay.db <catalog-pubkey-hex>
#   (deploy the new binary)
#   systemctl start relay
#
# An app id could hold a listing from the catalog key and another from its
# developer, one of them hidden. The new relay serves every stored event, so
# the app event must be one. The developer's listing wins: for each app id
# that has a listing from a pubkey other than the catalog key, this deletes the
# catalog key's kind 32267, 30063, and 3063 events for it. The indexer
# publishes a release again when the forge has one the developer has not.
# Apps with a single listing are untouched. Then events.hidden and its index are
# dropped.
#
# Before it changes anything, it writes a full copy of the database and a small
# database holding exactly the deleted events (table removed_events), next to
# relay.db. Nothing is deleted without both.
#
# Usage: drop-slots.sh [-y] <relay.db> <catalog-pubkey-hex>
set -euo pipefail

yes=0
if [ "${1:-}" = "-y" ]; then
	yes=1
	shift
fi
if [ $# -ne 2 ]; then
	echo "usage: $0 [-y] <relay.db> <catalog-pubkey-hex>" >&2
	exit 2
fi
db=$1
catalog=$2

command -v sqlite3 >/dev/null || { echo "sqlite3 is not installed" >&2; exit 1; }
[ -f "$db" ] || { echo "no such database: $db" >&2; exit 1; }
[[ $catalog =~ ^[0-9a-f]{64}$ ]] || { echo "the catalog pubkey must be 64 lowercase hex characters" >&2; exit 1; }
if command -v lsof >/dev/null && lsof "$db" >/dev/null 2>&1; then
	echo "$db is open by a running process; stop the relay first" >&2
	exit 1
fi
if [ -s "$db-wal" ]; then
	echo "$db-wal is not empty: the relay did not stop cleanly. Start and stop it once, then retry" >&2
	exit 1
fi

stamp=$(date +%Y%m%d-%H%M%S)
backup="$db.before-release-chain-$stamp"
removed="$db.removed-slot-events-$stamp.db"

has_events=$(sqlite3 "$db" "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='events'")
[ "$has_events" = "1" ] || { echo "$db has no events table" >&2; exit 1; }

# The apps to resolve and the catalog events to delete. Kept in a temp schema so
# the summary and the delete see the same rows.
select_sql="
CREATE TEMP TABLE apps AS
	SELECT t.value AS app FROM events e
	JOIN tags t ON t.event_id = e.id AND t.key = 'd'
	WHERE e.kind = 32267
	GROUP BY t.value
	HAVING SUM(e.pubkey = '$catalog') > 0 AND SUM(e.pubkey != '$catalog') > 0;
CREATE TEMP TABLE doomed AS
	SELECT DISTINCT e.id FROM events e
	JOIN tags t ON t.event_id = e.id
	WHERE e.pubkey = '$catalog' AND (
		(e.kind = 32267 AND t.key = 'd' AND t.value IN (SELECT app FROM apps))
		OR (e.kind IN (30063, 3063) AND t.key = 'i' AND t.value IN (SELECT app FROM apps))
	);
"

summary=$(sqlite3 "$db" "$select_sql
SELECT (SELECT COUNT(*) FROM apps) || ' apps with two listings, ' || (SELECT COUNT(*) FROM doomed) || ' catalog events to delete';")
echo "$summary"
echo "apps:"
sqlite3 "$db" "$select_sql SELECT '  ' || app FROM apps ORDER BY app;"
has_hidden=$(sqlite3 "$db" "SELECT COUNT(*) FROM pragma_table_info('events') WHERE name = 'hidden'")
[ "$has_hidden" = "1" ] && echo "events.hidden will be dropped" || echo "events.hidden is already gone"

if [ "$yes" -ne 1 ]; then
	read -r -p "Back up and apply? [y/N] " answer
	[ "$answer" = "y" ] || { echo "nothing changed"; exit 1; }
fi

sqlite3 "$db" "VACUUM INTO '$backup'"
sqlite3 "$backup" "PRAGMA integrity_check;" | grep -qx ok || { echo "the backup failed its integrity check" >&2; exit 1; }
echo "backup: $backup"

sqlite3 "$db" <<SQL
.bail on
ATTACH DATABASE '$removed' AS removed;
BEGIN IMMEDIATE;
$select_sql
CREATE TABLE removed.removed_events AS SELECT * FROM events WHERE id IN (SELECT id FROM doomed);
CREATE TABLE removed.removed_tags AS SELECT * FROM tags WHERE event_id IN (SELECT id FROM doomed);
DELETE FROM tags WHERE event_id IN (SELECT id FROM doomed);
DELETE FROM events WHERE id IN (SELECT id FROM doomed);
COMMIT;
DETACH DATABASE removed;
SQL
echo "removed events: $removed"

if [ "$has_hidden" = "1" ]; then
	sqlite3 "$db" <<'SQL'
.bail on
DROP INDEX IF EXISTS events_hidden_idx;
ALTER TABLE events DROP COLUMN hidden;
SQL
fi

left=$(sqlite3 "$db" "SELECT COUNT(*) FROM (
	SELECT t.value FROM events e JOIN tags t ON t.event_id = e.id AND t.key = 'd'
	WHERE e.kind = 32267 GROUP BY t.value HAVING COUNT(DISTINCT e.pubkey) > 1)")
echo "apps with more than one kind 32267 now: $left"
echo "done. Deploy the new relay and start it."
