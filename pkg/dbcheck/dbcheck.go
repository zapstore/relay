// Package dbcheck refuses a database written before a column the code needs.
//
// A schema file creates what is missing, but it cannot add a column to a table
// that exists, and the daemons do not migrate: relay/scripts/add-columns.sh does,
// once, with the relay stopped. A daemon that finds an unmigrated database says
// so, instead of failing on the first query that names the column.
package dbcheck

import (
	"database/sql"
	"fmt"
)

// RequireColumn returns an error when table exists but has no column. A table
// that does not exist is fine: the schema creates it with the column.
func RequireColumn(db *sql.DB, table, column string) error {
	var columns, present int
	err := db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(name = ?), 0) FROM pragma_table_info(?)`, column, table).Scan(&columns, &present)
	if err != nil {
		return fmt.Errorf("check %s: %w", table, err)
	}
	if columns > 0 && present == 0 {
		return fmt.Errorf("%s.%s is missing: the database was written by an older relay. Stop the relay and run relay/scripts/add-columns.sh on its data directory", table, column)
	}
	return nil
}
