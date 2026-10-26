-- Zapstore schema extensions for tag indexing.
-- Note: 'd' tags for kinds 30000-39999 are already indexed by the base schema.
--
-- Older builds kept an FTS5 index (apps_fts). Search is vector search now.
-- Drop the triggers so an insert does not write that index. The table can
-- stay: dropping it needs fts5, which this process is built without.

DROP TRIGGER IF EXISTS app_fts_ai;
DROP TRIGGER IF EXISTS app_fts_ad;

-- Pending events are the buffer zone: received but not yet promoted to the
-- main event table. A listing (kind 32267, 30063, and 3063) and a new active
-- kind 30509 stay here until promotion. Pending events are not served.
-- The app_id column and its index are added in Go: an older table lacks app_id.
CREATE TABLE IF NOT EXISTS pending_events (
    id          TEXT    PRIMARY KEY,    -- event id (sha256)
    kind        INTEGER NOT NULL,       -- event kind, for efficient filtering during promotion
    raw         TEXT    NOT NULL,       -- full event JSON
    received_at INTEGER NOT NULL        -- unix timestamp of when we received the event
);

CREATE INDEX IF NOT EXISTS idx_pending_events_kind        ON pending_events(kind);
CREATE INDEX IF NOT EXISTS idx_pending_events_received_at ON pending_events(received_at);

-- Slot selection is events.hidden now, added in Go.
DROP TRIGGER IF EXISTS listings_ai;
DROP TRIGGER IF EXISTS listings_ad;
DROP TABLE IF EXISTS listings;

DROP TABLE IF EXISTS read_certificate;
DROP TABLE IF EXISTS apk_signing;
DROP TABLE IF EXISTS app_certificate;

-- Deletes the tags of a deleted event. The foreign key cascade does the same,
-- but only on a connection with foreign keys on.
CREATE TRIGGER IF NOT EXISTS tags_ad AFTER DELETE ON events
BEGIN
	DELETE FROM tags WHERE event_id = OLD.id;
END;

-- Universal single-letter tag indexing for all event kinds.
-- Covers tags like a, e, f, i, p, t, x, A, E, K, P, etc.
-- The base schema already indexes 'd' for addressable kinds; INSERT OR IGNORE deduplicates.
CREATE TRIGGER IF NOT EXISTS single_letter_tags_ai AFTER INSERT ON events
BEGIN
	INSERT OR IGNORE INTO tags (event_id, key, value)
	SELECT NEW.id, json_extract(value, '$[0]'), json_extract(value, '$[1]')
	FROM json_each(NEW.tags)
	WHERE json_type(value) = 'array'
		AND json_array_length(value) > 1
		AND length(json_extract(value, '$[0]')) = 1;
END;

-- KindApp (32267) - multi-character tag indexing
CREATE TRIGGER IF NOT EXISTS app_tags_ai AFTER INSERT ON events
WHEN NEW.kind = 32267
BEGIN
	INSERT OR IGNORE INTO tags (event_id, key, value)
	SELECT NEW.id, json_extract(value, '$[0]'), json_extract(value, '$[1]')
	FROM json_each(NEW.tags)
	WHERE json_type(value) = 'array'
		AND json_array_length(value) > 1
		AND json_extract(value, '$[0]') IN ('name', 'license', 'url', 'repository');
END;

-- KindRelease (30063) - multi-character tag indexing
CREATE TRIGGER IF NOT EXISTS release_tags_ai AFTER INSERT ON events
WHEN NEW.kind = 30063
BEGIN
	INSERT OR IGNORE INTO tags (event_id, key, value)
	SELECT NEW.id, json_extract(value, '$[0]'), json_extract(value, '$[1]')
	FROM json_each(NEW.tags)
	WHERE json_type(value) = 'array'
		AND json_array_length(value) > 1
		AND json_extract(value, '$[0]') IN ('version', 'commit');
END;

-- KindAsset (3063) - multi-character tag indexing
DROP TRIGGER IF EXISTS asset_tags_ai;
CREATE TRIGGER asset_tags_ai AFTER INSERT ON events
WHEN NEW.kind = 3063
BEGIN
	INSERT OR IGNORE INTO tags (event_id, key, value)
	SELECT NEW.id, json_extract(value, '$[0]'), json_extract(value, '$[1]')
	FROM json_each(NEW.tags)
	WHERE json_type(value) = 'array'
		AND json_array_length(value) > 1
		AND json_extract(value, '$[0]') IN ('url', 'version', 'apk_certificate_hash', 'apk_certificate_ancestor');
END;

-- KindFile (1063) - multi-character tag indexing
CREATE TRIGGER IF NOT EXISTS file_tags_ai AFTER INSERT ON events
WHEN NEW.kind = 1063
BEGIN
	INSERT OR IGNORE INTO tags (event_id, key, value)
	SELECT NEW.id, json_extract(value, '$[0]'), json_extract(value, '$[1]')
	FROM json_each(NEW.tags)
	WHERE json_type(value) = 'array'
		AND json_array_length(value) > 1
		AND json_extract(value, '$[0]') IN ('url', 'fallback', 'version', 'apk_signature_hash');
END;
