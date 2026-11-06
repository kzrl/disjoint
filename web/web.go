package web

import (
	"embed"
	"mime"
)

// Files contains the embedded static assets and HTML templates.
//
//go:embed static/* templates/*
var Files embed.FS

func init() {
	// Go's built-in MIME table has no entry for .webmanifest, and whether the
	// host's /etc/mime.types does is not something the binary can rely on.
	// Browsers reject a manifest served as text/plain, so register it here
	// once for the whole process (the embedded file server then labels
	// /static/site.webmanifest correctly).
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}
