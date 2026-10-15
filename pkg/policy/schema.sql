CREATE TABLE IF NOT EXISTS blacklist (
    kind       TEXT    NOT NULL,
    id         TEXT    NOT NULL,
    reason     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (kind, id)
);

CREATE TABLE IF NOT EXISTS banned_hashes (
    hash   TEXT PRIMARY KEY,
    reason TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ban_notices (
    app_id TEXT PRIMARY KEY,
    reason TEXT NOT NULL
);

DROP TABLE IF EXISTS admissions;
DROP TABLE IF EXISTS allowed;
DROP TABLE IF EXISTS paid_access;
DROP TABLE IF EXISTS suggestions;
