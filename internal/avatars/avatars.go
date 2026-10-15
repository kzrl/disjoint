// Package avatars generates deterministic retro pixel-art avatars from a
// username. Each avatar is a size x size grid of cells: the left half is
// drawn from a seed derived from the username and mirrored onto the right
// half, so every sprite is bilaterally symmetric. Cells are painted with up
// to three colors from the bright half of the classic ANSI 16-color palette
// (one color dominates so each avatar reads as a single figure rather than
// confetti). The output is an SVG with a transparent background and crisp
// edges, sized by CSS.
//
// Render is a pure function: the same username always produces
// byte-identical output, and the username is only hashed, never embedded in
// the SVG.
package avatars

import (
	"hash/fnv"
	"strconv"
	"strings"
)

// size is the full grid dimension in cells. The left half (size/2 columns)
// is generated and mirrored for the right half.
const size = 12

// maxAttempts bounds the resample loop that rejects all-empty and all-full
// grids (each has probability ~1e-20 for size=12, so this is belt-and-braces
// and the loop effectively never fires twice).
const maxAttempts = 8

// fillPercent is the percentage chance of each left-half cell being painted.
const fillPercent = 45

// versionSalt is mixed into the seed so a future generator revision can
// re-roll the whole family without ambiguity with old renderings.
const versionSalt = "disjoint-avatar-v1"

// palette is the bright half (indices 9-15) of the classic ANSI 16-color
// palette. Avatars use a transparent background, so only the bright colors
// are used: they read on both the dark and light themes.
var palette = []string{
	"#ff0000", // 9:  bright red
	"#00ff00", // 10: bright green
	"#ffff00", // 11: bright yellow
	"#0000ff", // 12: bright blue
	"#ff00ff", // 13: bright magenta
	"#00ffff", // 14: bright cyan
	"#ffffff", // 15: bright white
}

// Render returns the deterministic pixel-art SVG avatar for username and
// nonce. Nonce is the user's avatar_nonce (0 until they shuffle), so the
// same username always renders the same sprite, and shuffling re-rolls it.
func Render(username string, nonce int64) string {
	rng := newRng(hash(username, nonce))
	colors := pickColors(rng)

	var half [size][size / 2]string
	filled := 0
	for attempt := 0; ; attempt++ {
		filled = 0
		for y := 0; y < size; y++ {
			for x := 0; x < size/2; x++ {
				if rng.next()%100 < fillPercent {
					half[y][x] = pickColor(rng, colors)
					filled++
				} else {
					half[y][x] = ""
				}
			}
		}
		if filled > 0 && filled < size*size/2 || attempt == maxAttempts-1 {
			break
		}
	}
	return renderSVG(half)
}

// hash derives the PRNG seed: FNV-1a over the version salt, the exact
// username (disjoint usernames are case-sensitive), and the avatar nonce.
// The NUL separators keep (username, nonce) pairs unambiguous.
func hash(username string, nonce int64) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(versionSalt + "\x00" + username + "\x00" + strconv.FormatInt(nonce, 10)))
	return h.Sum64()
}

// splitMix64 is a tiny, fast, deterministic 64-bit PRNG. It is used instead
// of math/rand so output depends only on the seed, on no global state, and
// stays stable across Go versions and platforms.
type splitMix64 struct{ state uint64 }

func newRng(seed uint64) *splitMix64 { return &splitMix64{state: seed} }

func (r *splitMix64) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// pickColors chooses three distinct palette colors for the avatar.
func pickColors(r *splitMix64) [3]string {
	perm := make([]int, len(palette))
	for i := range perm {
		perm[i] = i
	}
	for i := len(perm) - 1; i > 0; i-- {
		j := int(r.next() % uint64(i+1))
		perm[i], perm[j] = perm[j], perm[i]
	}
	return [3]string{palette[perm[0]], palette[perm[1]], palette[perm[2]]}
}

// pickColor picks a fill for one cell, biased toward colors[0] so the
// avatar reads as a single figure: 60% dominant, 30% secondary, 10% accent.
func pickColor(r *splitMix64, colors [3]string) string {
	switch r.next() % 10 {
	case 0, 1, 2, 3, 4:
		return colors[0]
	case 5, 6, 7:
		return colors[1]
	default:
		return colors[2]
	}
}

// cellAt resolves the color of a full-grid cell, mapping right-half columns
// back onto their mirrored left-half counterpart.
func cellAt(half [size][size / 2]string, y, x int) string {
	if x >= size/2 {
		x = size - 1 - x
	}
	return half[y][x]
}

// renderSVG emits the sprite as SVG, one rect per horizontal run of
// same-colored cells (run-length merged, typically ~30-50 rects total).
// Width/height are left unset so CSS (.avatar-sm / .avatar-lg) controls the
// display size; crisp edges keep pixels sharp at any scale.
func renderSVG(half [size][size / 2]string) string {
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(size))
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(size))
	b.WriteString(`" shape-rendering="crispEdges" aria-hidden="true">`)
	for y := 0; y < size; y++ {
		for x := 0; x < size; {
			col := cellAt(half, y, x)
			if col == "" {
				x++
				continue
			}
			start := x
			x++
			for x < size && cellAt(half, y, x) == col {
				x++
			}
			b.WriteString(`<rect x="`)
			b.WriteString(strconv.Itoa(start))
			b.WriteString(`" y="`)
			b.WriteString(strconv.Itoa(y))
			b.WriteString(`" width="`)
			b.WriteString(strconv.Itoa(x - start))
			b.WriteString(`" height="1" fill="`)
			b.WriteString(col)
			b.WriteString(`"/>`)
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}
