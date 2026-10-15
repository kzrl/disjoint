package avatars

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
)

// parseCells decodes the rects of a rendered SVG into a "x,y -> fill" map,
// covering every painted cell exactly once.
func parseCells(t *testing.T, svg string) map[string]string {
	t.Helper()
	var doc struct {
		XMLName xml.Name `xml:"svg"`
		Rects   []struct {
			X      int    `xml:"x,attr"`
			Y      int    `xml:"y,attr"`
			Width  int    `xml:"width,attr"`
			Height int    `xml:"height,attr"`
			Fill   string `xml:"fill,attr"`
		} `xml:"rect"`
	}
	if err := xml.Unmarshal([]byte(svg), &doc); err != nil {
		t.Fatalf("SVG is not well-formed XML: %v", err)
	}
	if doc.XMLName.Local != "svg" {
		t.Fatalf("root element is %q, want svg", doc.XMLName.Local)
	}
	cells := map[string]string{}
	for _, r := range doc.Rects {
		if r.Height != 1 {
			t.Fatalf("rect y=%d has height %d, want 1", r.Y, r.Height)
		}
		for x := r.X; x < r.X+r.Width; x++ {
			key := fmt.Sprintf("%d,%d", x, r.Y)
			if prev, ok := cells[key]; ok {
				t.Fatalf("cell %s painted twice: %q then %q", key, prev, r.Fill)
			}
			cells[key] = r.Fill
		}
	}
	return cells
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
	"alice": "2c1f956262a0a30194129d778082bfa2635a7f8159c50792ac069980ced811ac",
	"Bob":   "0b87913721d0d912456a5686a048193c691528fbc17213482d62025e1c43498e",
	"Ω":     "b98af1509f15c9ce680d3e3f9400f27301964ce94825f5ffe5af2b050bbda372",
}

// TestRenderDigest pins the generator output so an accidental PRNG, palette,
// or threshold change fails loudly instead of silently re-rolling every
// avatar in the fleet.
func TestRenderDigest(t *testing.T) {
	for name, want := range pinnedDigests {
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
		wantPrologue := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"`, size, size)
		if !strings.HasPrefix(svg, wantPrologue) {
			t.Errorf("%s: unexpected svg prologue: %.80s…", name, svg)
		}
	}
}

func TestRenderSymmetricAndPalette(t *testing.T) {
	for _, name := range []string{"alice", "Bob", "Ω", "disjoint"} {
		cells := parseCells(t, Render(name, 0))
		if len(cells) == 0 || len(cells) > size*size {
			t.Fatalf("%s: painted %d cells, want 1..%d", name, len(cells), size*size)
		}
		for key, fill := range cells {
			var x, y int
			fmt.Sscanf(key, "%d,%d", &x, &y)
			mirror := size - 1 - x
			mkey := fmt.Sprintf("%d,%d", mirror, y)
			other, ok := cells[mkey]
			if !ok || other != fill {
				t.Fatalf("%s: cell %s (fill %q) not mirrored at %s (fill %q)", name, key, fill, mkey, other)
			}
			if !isPaletteColor(fill) {
				t.Fatalf("%s: cell %s has fill %q outside the ANSI palette", name, key, fill)
			}
		}
	}
}

func isPaletteColor(c string) bool {
	for _, p := range palette {
		if p == c {
			return true
		}
	}
	return false
}
