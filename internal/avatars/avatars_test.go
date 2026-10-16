package avatars

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"testing"
)

// rect is one decoded <rect> of a rendered SVG.
type rect struct {
	X, Y, W, H, RX float64
	Fill           string
}

// parseRects decodes every rect of a rendered SVG at any depth, validating
// well-formedness on the way. A rect's effective fill is its own fill
// attribute, or the fill of the innermost enclosing <g> when unset.
func parseRects(t *testing.T, svg string) []rect {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(svg))
	var root string
	var groupFills []string
	var out []rect
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("SVG is not well-formed XML: %v", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			if root == "" {
				root = tok.Name.Local
				if root != "svg" {
					t.Fatalf("root element is %q, want svg", root)
				}
				continue
			}
			if tok.Name.Local == "g" {
				groupFills = append(groupFills, attr(tok.Attr, "fill"))
				continue
			}
			if tok.Name.Local != "rect" {
				continue
			}
			var r rect
			for _, a := range tok.Attr {
				v, _ := strconv.ParseFloat(a.Value, 64)
				switch a.Name.Local {
				case "x":
					r.X = v
				case "y":
					r.Y = v
				case "width":
					r.W = v
				case "height":
					r.H = v
				case "rx":
					r.RX = v
				case "fill":
					r.Fill = a.Value
				}
			}
			if r.Fill == "" && len(groupFills) > 0 {
				r.Fill = groupFills[len(groupFills)-1]
			}
			out = append(out, r)
		case xml.EndElement:
			if tok.Name.Local == "g" && len(groupFills) > 0 {
				groupFills = groupFills[:len(groupFills)-1]
			}
		}
	}
	return out
}

func attr(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func TestRenderDeterministic(t *testing.T) {
	for _, name := range []string{"alice", "Bob", "Ω"} {
		if Render(name, 0) != Render(name, 0) {
			t.Fatalf("Render(%q) is not deterministic", name)
		}
	}
}

func TestRenderDistinct(t *testing.T) {
	a := Render("alice", 0)
	if b := Render("bob", 0); b == a {
		t.Fatal("different usernames rendered the same SVG")
	}
	if upper := Render("Alice", 0); upper == a {
		t.Fatal("case-different usernames rendered the same SVG (usernames are case-sensitive)")
	}
}

// pinnedDigests are the committed fixtures for the generator. Update them
// deliberately when revising the generator (and bump versionSalt).
var pinnedDigests = map[string]string{
	"alice": "099b743a4a5deac38e73fe1845fb31eee4766e672df521b6e95a5702c37af567",
	"Bob":   "9511c9b8cf7be72fdaaa51f58891334b65d2f3d184583e8c8935dbb3a1fac420",
	"Ω":     "06e807d4efba3075fe76c3b35111ffb2e88192bdc8fd15c8fb2724edb240fdb4",
}

// TestRenderDigest pins the generator output so an accidental PRNG, palette,
// or shape change fails loudly instead of silently re-rolling every avatar
// in the fleet.
func TestRenderDigest(t *testing.T) {
	for name, want := range pinnedDigests {
		if want == "REPLACE" {
			t.Skip("digests not pinned yet")
		}
		if got := sha256hex(Render(name, 0)); got != want {
			t.Errorf("Render(%q) digest = %s, want %s (generator changed? update the fixture and bump versionSalt)", name, got, want)
		}
	}
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestRenderNonceReRolls(t *testing.T) {
	base := Render("alice", 0)
	differs := 0
	for nonce := int64(1); nonce <= 5; nonce++ {
		if got := Render("alice", nonce); got != base {
			differs++
		}
		if got := Render("alice", nonce); got != Render("alice", nonce) {
			t.Fatalf("Render with nonce %d is not deterministic", nonce)
		}
	}
	if differs == 0 {
		t.Fatal("shuffling never changed the avatar across 5 nonces")
	}
	// The NUL separators keep (username, nonce) pairs unambiguous: the seed
	// for ("ab", 12) must not equal the seed for ("ab1", 2).
	if Render("ab", 12) == Render("ab1", 2) {
		t.Error("seed ambiguity: (ab,12) rendered the same as (ab1,2)")
	}
}

func TestRenderPrologue(t *testing.T) {
	for _, name := range []string{"alice", "x", "a-very-long-username-with-dashes.123"} {
		svg := Render(name, 0)
		wantPrologue := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" aria-hidden="true">`
		if !strings.HasPrefix(svg, wantPrologue) {
			t.Errorf("%s: unexpected svg prologue: %.80s…", name, svg)
		}
	}
}

// TestRenderStructure checks the anatomy of every render: one ground rect,
// the full 144-dot matrix, and a face made only of lit pixels at the
// variant coordinates, in palette colors.
func TestRenderStructure(t *testing.T) {
	for _, name := range []string{"alice", "Bob", "Ω", "disjoint"} {
		rects := parseRects(t, Render(name, 0))

		if len(rects) < 2 {
			t.Fatalf("%s: only %d rects", name, len(rects))
		}

		// Ground: first rect, full canvas, background palette color.
		g := rects[0]
		if g.X != 0 || g.Y != 0 || g.W != canvasSize || g.H != canvasSize || g.Fill == "" {
			t.Fatalf("%s: ground rect is (%v %v %v %v %q), want full canvas with a fill", name, g.X, g.Y, g.W, g.H, g.Fill)
		}
		if !inList(backgrounds, g.Fill) {
			t.Fatalf("%s: ground fill %q is not a background color", name, g.Fill)
		}

		// Dot matrix: next 144 rects, all dark semi-transparent pixels.
		dots := rects[1 : 1+dotCount()]
		if len(dots) != dotCount() {
			t.Fatalf("%s: %d dot rects, want %d", name, len(dots), dotCount())
		}
		for i, p := range dots {
			if p.W != pixelSize || p.H != pixelSize || p.RX != pixelRadius {
				t.Fatalf("%s: dot %d has wrong pixel geometry (%v %v rx %v)", name, i, p.W, p.H, p.RX)
			}
			if !inCoord(dotXs[:], p.X) || !inCoord(dotYs[:], p.Y) {
				t.Fatalf("%s: dot %d at (%v,%v) is off the dot matrix", name, i, p.X, p.Y)
			}
		}

		// Face: the remaining rects are lit pixels, all one glow color, at
		// eyes/mouth variant coordinates.
		glow := ""
		face := 0
		for _, p := range rects[1+dotCount():] {
			if glow == "" {
				glow = p.Fill
			} else if p.Fill != glow {
				t.Fatalf("%s: face mixes fills %q and %q", name, glow, p.Fill)
			}
			if !inList(glows, p.Fill) {
				t.Fatalf("%s: face fill %q is not a glow color", name, p.Fill)
			}
			if !isFaceCoord(p.X, p.Y) {
				t.Fatalf("%s: face pixel at (%v,%v) is off the face grid", name, p.X, p.Y)
			}
			if p.W != pixelSize || p.H != pixelSize || p.RX != pixelRadius {
				t.Fatalf("%s: face pixel has wrong geometry (%v %v rx %v)", name, p.W, p.H, p.RX)
			}
			face++
		}
		// Every variant has at least 4 lit pixels per part.
		if face < 8 {
			t.Fatalf("%s: only %d face pixels, want at least 8", name, face)
		}
	}
}

// TestRenderVariety sanity-checks that the picker actually ranges over the
// palettes and variant sets (a stuck PRNG would render one face for everyone).
func TestRenderVariety(t *testing.T) {
	names := []string{
		"alice", "bob", "carol", "dave", "erin", "frank",
		"grace", "heidi", "ivan", "judy", "mallory", "niaj",
		"oleg", "pepper", "quinn", "rupert", "sybil", "trent",
		"ursula", "victor", "wendy", "xavier", "yara", "zane",
	}
	seenGlow := map[string]bool{}
	seenBg := map[string]bool{}
	seenFaces := map[string]bool{}
	for _, n := range names {
		svg := Render(n, 0)
		rects := parseRects(t, svg)
		g := rects[0]
		seenBg[g.Fill] = true
		face := rects[1+dotCount():]
		glow := face[0].Fill
		seenGlow[glow] = true
		// The face section of the SVG is a stable signature of the
		// (glow, eyes, mouth) choice.
		i := strings.Index(svg, `<g fill="`+glow+`">`)
		seenFaces[svg[i:]] = true
	}
	if len(seenGlow) < 3 {
		t.Errorf("only %d distinct glow colors across %d users, want more", len(seenGlow), len(names))
	}
	if len(seenBg) < 3 {
		t.Errorf("only %d distinct background colors across %d users, want more", len(seenBg), len(names))
	}
	if len(seenFaces) < 5 {
		t.Errorf("only %d distinct face renders across %d users, want more", len(seenFaces), len(names))
	}
}

func dotCount() int { return len(dotXs) * len(dotYs) }

func inList(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func inCoord(coords []float64, v float64) bool {
	for _, c := range coords {
		if c == v {
			return true
		}
	}
	return false
}

// isFaceCoord reports whether (x, y) is a valid eyes or mouth pixel position.
func isFaceCoord(x, y float64) bool {
	if inCoord(eyesXs[:], x) && inCoord(eyesYs[:], y) {
		return true
	}
	return inCoord(mouthXs[:], x) && inCoord(mouthYs[:], y)
}
