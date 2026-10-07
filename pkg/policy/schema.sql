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

-- Paid publishers. Not an allow-list: it does not bypass the blacklist.
CREATE TABLE IF NOT EXISTS paid_access (
    kind       TEXT    NOT NULL,
    id         TEXT    NOT NULL,
    reason     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (kind, id)
);

DROP TABLE IF EXISTS admissions;

CREATE TABLE IF NOT EXISTS suggestions (
    id               TEXT    PRIMARY KEY,
    repository       TEXT    NOT NULL,
    release_source   TEXT    NOT NULL,
    certificate_hash TEXT    NOT NULL,
    created_at       INTEGER NOT NULL
);
