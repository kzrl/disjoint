package handlers

import (
	"log"
	"net/http"
)

// audit writes a small, structured security-relevant action record. It never
// logs query strings, message bodies, or credentials.
func audit(r *http.Request, actorID int64, action string, targetID int64) {
	log.Printf("[audit] actor=%d action=%s target=%d path=%q", actorID, action, targetID, r.URL.Path)
}
