package db

import (
	"errors"

	"modernc.org/sqlite"
)

const (
	sqliteConstraintMask = 0xff
	sqliteConstraintCode = 19
)

// IsDuplicateConstraint reports SQLITE_CONSTRAINT-family errors (any extended
// code sharing the base number), which covers unique/PK violations.
func IsDuplicateConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}

	return (sqliteErr.Code() & sqliteConstraintMask) == sqliteConstraintCode
}
