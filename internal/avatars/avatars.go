// Package avatars generates deterministic "pixelbot"-style pixel avatars
// from a username: a glowing robot face (one of several eye and mouth
// shapes) drawn from lit rounded pixels on a dark 12x12 dot-matrix ground,
// in the manner of the DiceBear "pixelbot" style. Shapes, coordinates, and
// palettes come from shapes.go (extracted from the style definition); the
// per-avatar choices are drawn from a seed derived from the username and a
// nonce.
//
// Render is a pure function: the same (username, nonce) always produces
// byte-identical output, and the username is only hashed, never embedded in
// the SVG.
package avatars

import (
	"hash/fnv"
	"strconv"
	"strings"
)

// canvasSize is the SVG viewBox dimension; the sprite is drawn in a
// canvasSize x canvasSize coordinate space.
const canvasSize = 100

// versionSalt is mixed into the seed so a future generator revision can
// re-roll the whole family without ambiguity with old renderings. v2 is the
// pixelbot-style renderer (v1 was the mirrored confetti sprite).
const versionSalt = "disjoint-avatar-v2"

// dotMatrix is the static background grid, identical for every avatar: the
// full 12x12 field of dark, semi-transparent rounded pixels.
var dotMatrix = buildDotMatrix()

// Render returns the deterministic pixelbot-style SVG avatar for username
// and nonce. Nonce is the user's avatar_nonce (0 until they shuffle), so
// the same username always renders the same sprite, and shuffling re-rolls
// it.
func Render(username string, nonce int64) string {
	rng := newRng(hash(username, nonce))
	bg := backgrounds[rng.intn(len(backgrounds))]
	glow := glows[rng.intn(len(glows))]
	eyes := eyesVariants[rng.intn(len(eyesVariants))]
	mouth := mouthVariants[rng.intn(len(mouthVariants))]
	return renderSVG(bg, glow, eyes, mouth)
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

// intn returns a uniform-ish index in [0, n). The modulo bias is at most
// 2^-64 relative and irrelevant for palette and variant picks.
func (r *splitMix64) intn(n int) int { return int(r.next() % uint64(n)) }

// buildDotMatrix renders the 12x12 background grid once; every avatar
// embeds the same string.
func buildDotMatrix() string {
	var b strings.Builder
	b.WriteString(`<g fill="#000000" fill-opacity=".4">`)
	for _, y := range dotYs {
		for _, x := range dotXs {
			writePixel(&b, x, y)
		}
	}
	b.WriteString(`</g>`)
	return b.String()
}

// writePixel appends one lit pixel: a pixelSize square with pixelRadius
// corners (the same shape the style definition draws as arc paths).
func writePixel(b *strings.Builder, x, y float64) {
	b.WriteString(`<rect x="`)
	b.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
	b.WriteString(`" y="`)
	b.WriteString(strconv.FormatFloat(y, 'f', -1, 64))
	b.WriteString(`" width="`)
	b.WriteString(strconv.FormatFloat(pixelSize, 'f', -1, 64))
	b.WriteString(`" height="`)
	b.WriteString(strconv.FormatFloat(pixelSize, 'f', -1, 64))
	b.WriteString(`" rx="`)
	b.WriteString(strconv.FormatFloat(pixelRadius, 'f', -1, 64))
	b.WriteString(`"/>`)
}

// renderSVG assembles the avatar: a solid ground rect, the dot matrix, and
// the lit face pixels in the glow color. Width/height are left unset so CSS
// (.avatar-sm / .avatar-lg) controls the display size.
func renderSVG(bg, glow, eyes, mouth string) string {
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 `)
	b.WriteString(strconv.Itoa(canvasSize))
	b.WriteString(` `)
	b.WriteString(strconv.Itoa(canvasSize))
	b.WriteString(`" aria-hidden="true">`)
	b.WriteString(`<rect width="`)
	b.WriteString(strconv.Itoa(canvasSize))
	b.WriteString(`" height="`)
	b.WriteString(strconv.Itoa(canvasSize))
	b.WriteString(`" fill="`)
	b.WriteString(bg)
	b.WriteString(`"/>`)
	b.WriteString(dotMatrix)
	b.WriteString(`<g fill="`)
	b.WriteString(glow)
	b.WriteString(`">`)
	writeFace(&b, eyesXs[:], eyesYs[:], eyes)
	writeFace(&b, mouthXs[:], mouthYs[:], mouth)
	b.WriteString(`</g></svg>`)
	return b.String()
}

// writeFace emits the lit pixels of one face variant, a grid of
// "#" (lit) and "." (dark) rows indexed onto xs/ys.
func writeFace(b *strings.Builder, xs, ys []float64, grid string) {
	for row, line := range strings.Split(grid, "\n") {
		for col, ch := range line {
			if ch == '#' {
				writePixel(b, xs[col], ys[row])
			}
		}
	}
}
