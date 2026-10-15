// Command avatar-preview renders the default pixel avatars for a set of
// sample usernames into a local HTML contact sheet for visual review of the
// generator's look. Usage:
//
//	go run ./tools/avatar-preview [-o out.html] [-nonce n] [username ...]
//
// With no username arguments it uses a built-in sample covering the edge
// cases (short, long, mixed case, digits).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"disjoint/internal/avatars"
)

var sampleNames = []string{
	"alice", "Bob", "carol99", "D", "x1", "ZedZedZed",
	"aaaaaaaa", "AbCdEfGh", "0o0o0o", "MixedCaseName",
	"disjoint", "pixel", "ansi", "retro",
	"terminal", "phosphor", "green", "blue",
	"cyan", "magenta", "yellow", "white",
	"blackjack", "longusernamefortesting1234",
}

func main() {
	out := flag.String("o", "avatar-preview.html", "output HTML file")
	nonce := flag.Int64("nonce", 0, "avatar nonce to render (0 = original roll)")
	flag.Parse()
	names := sampleNames
	if flag.NArg() > 0 {
		names = flag.Args()
	}

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Avatar preview</title>
<style>
body { margin: 0; font-family: monospace; }
section { padding: 2rem; }
section h1 { font-size: 1rem; }
#dark { background: #1a1b26; color: #c0c5d0; }
#light { background: #f5f5f5; color: #333; }
.grid { display: flex; flex-wrap: wrap; gap: 1.5rem; }
.cell { text-align: center; }
.cell img { display: block; margin: 0 auto .5rem; image-rendering: pixelated; }
.cell span { font-size: .75rem; }
</style></head><body>`)
	for _, theme := range []struct{ id, label string }{{"dark", "dark background"}, {"light", "light background"}} {
		b.WriteString(fmt.Sprintf(`<section id="%s"><h1>%s</h1><div class="grid">`, theme.id, theme.label))
		for _, name := range names {
			svg := avatars.Render(name, *nonce)
			b.WriteString(`<div class="cell">`)
			for _, px := range []int{12, 48, 144} {
				b.WriteString(fmt.Sprintf(`<img src="data:image/svg+xml,%s" width="%d" height="%d">`,
					strings.ReplaceAll(strings.ReplaceAll(svg, `"`, "%22"), `#`, "%23"), px, px))
			}
			b.WriteString(`<span>` + fmtHTML(name) + `</span></div>`)
		}
		b.WriteString(`</div></section>`)
	}
	b.WriteString(`</body></html>`)

	if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d usernames)\n", *out, len(names))
}

func fmtHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
