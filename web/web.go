package web

import "embed"

// Files contains the embedded static assets and HTML templates.
//
//go:embed static/* templates/*
var Files embed.FS
