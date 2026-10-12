package database

import "time"

// ParseTimestamp parses the text values SQLite stores in DATETIME columns
// (CURRENT_TIMESTAMP produces "YYYY-MM-DD HH:MM:SS"; fractional seconds
// and time.Time-written values are tolerated). It is the single place that
// understands on-disk timestamp formats, so handler code only ever sees
// time.Time.
func ParseTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999999-07:00",
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
