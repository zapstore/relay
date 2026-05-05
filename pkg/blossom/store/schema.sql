CREATE TABLE IF NOT EXISTS blobs (
    hash        TEXT    PRIMARY KEY,    -- sha256 stored as a hexadecimal
    type        TEXT    NOT NULL,       -- content type of the blob e.g. text/plain charset=utf-8
    size        INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    auth_pubkey TEXT                    -- hex pubkey that authenticated the upload, NULL if unknown
);

CREATE INDEX IF NOT EXISTS idx_blobs_auth_pubkey ON blobs(auth_pubkey);
CREATE INDEX IF NOT EXISTS idx_blobs_type        ON blobs(type);
