package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const logSchema = `
CREATE TABLE IF NOT EXISTS admin_log (
    at       INTEGER NOT NULL,
    pubkey   TEXT    NOT NULL,
    op       TEXT    NOT NULL,
    args     TEXT    NOT NULL,
    event_id TEXT    NOT NULL,
    ok       INTEGER NOT NULL,
    error    TEXT    NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS admin_log_event ON admin_log (event_id) WHERE event_id != '';
CREATE INDEX IF NOT EXISTS admin_log_at ON admin_log (at);
`

var errReplay = errors.New("this signed command was already used")

// entry is one authorized write. pubkey and eventID are empty for the relay's own user.
type entry struct {
	at      int64
	pubkey  string
	op      string
	args    args
	eventID string
}

func openLog(db *sql.DB) error {
	_, err := db.Exec(logSchema)
	return err
}

// reserve writes the entry before the command runs. The unique event id
// makes a second use of the same signed event fail here, before any change.
func reserve(ctx context.Context, db *sql.DB, e entry) (int64, error) {
	raw, err := json.Marshal(e.args)
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO admin_log (at, pubkey, op, args, event_id, ok, error)
		VALUES (?, ?, ?, ?, ?, 0, 'running')
	`, e.at, e.pubkey, e.op, string(raw), e.eventID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, errReplay
		}
		return 0, err
	}
	return res.LastInsertId()
}

// settle records how the command ended.
func settle(db *sql.DB, row int64, cmdErr error) error {
	ok, msg := 1, ""
	if cmdErr != nil {
		ok, msg = 0, cmdErr.Error()
	}
	_, err := db.Exec(`UPDATE admin_log SET ok = ?, error = ? WHERE rowid = ?`, ok, msg, row)
	return err
}
