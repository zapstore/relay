-- Backfill single-letter tags for all existing events.
-- Runs once; tracked by the migrations table (version 1).
INSERT OR IGNORE INTO tags (event_id, key, value)
SELECT e.id, json_extract(t.value, '$[0]'), json_extract(t.value, '$[1]')
FROM events e, json_each(e.tags) t
WHERE json_type(t.value) = 'array'
	AND json_array_length(t.value) > 1
	AND length(json_extract(t.value, '$[0]')) = 1;
