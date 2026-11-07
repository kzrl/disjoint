package database

import "database/sql"

// DB exposes the underlying handle so callers that need a transaction across
// several generated queries (e.g. passkey signup activation) can begin one.
// The generated Queries type keeps db unexported; this accessor lives outside
// the generated files so `make generate` cannot clobber it.
func (q *Queries) DB() *sql.DB {
	if db, ok := q.db.(*sql.DB); ok {
		return db
	}
	return nil
}
