package database

import "time"

// LatestUpdatedTime interprets the LatestUpdatedAt aggregate from
// ListBoardsWithDetails. sqlc cannot type a MAX() expression, so the column
// arrives as interface{} (string in practice, nil for boards without
// threads). This method is the single place that turns it into a time.Time,
// keeping handlers free of type switches.
func (r ListBoardsWithDetailsRow) LatestUpdatedTime() (time.Time, bool) {
	switch v := r.LatestUpdatedAt.(type) {
	case string:
		return ParseTimestamp(v)
	case time.Time:
		return v, true
	case nil:
		return time.Time{}, false
	default:
		return time.Time{}, false
	}
}
