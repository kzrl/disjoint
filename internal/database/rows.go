package database

import "time"

// LatestUpdatedTime interprets the LatestUpdatedAt aggregate from
// ListBoardsWithDetails. The SQL COALESCEs NULL (boards without threads)
// to the empty string, so this simply reports false for those.
func (r ListBoardsWithDetailsRow) LatestUpdatedTime() (time.Time, bool) {
	return ParseTimestamp(r.LatestUpdatedAt)
}
