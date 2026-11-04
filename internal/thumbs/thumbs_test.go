package thumbs

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writePNG writes a real PNG (so DecodeConfig/Decode work) and returns its path.
// transparent fills every pixel with alpha 0 when true, opaque alpha 255
// otherwise.
func writePNG(t *testing.T, dir, name string, w, h int, transparent bool) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	a := uint8(0xff)
	if transparent {
		a = 0
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x % 256), G: uint8(y % 256), B: 64, A: a})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("failed to encode test png: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("failed to write test png: %v", err)
	}
	return p
}

// oversizedPNGHeader builds a valid PNG signature + IHDR claiming huge
// dimensions, with no image data. image.DecodeConfig reads only the header, so
// this exercises the pixel cap without allocating a 100000x100000 image.
func oversizedPNGHeader(width, height uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // color type: truecolor with alpha
	// compression, filter, interlace are all 0

	chunk := append([]byte("IHDR"), ihdr...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(ihdr)))
	buf.Write(length[:])
	buf.Write(chunk)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(chunk))
	buf.Write(crc[:])
	return buf.Bytes()
}

func decodeFile(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open %s: %v", path, err)
	}
	defer f.Close()
	img, format, err := image.Decode(f)
	if err != nil {
		t.Fatalf("failed to decode %s: %v", path, err)
	}
	t.Logf("decoded %s as %s", filepath.Base(path), format)
	return img
}

func TestEnsureScalesPreservingAspect(t *testing.T) {
	dir := t.TempDir()
	name := "photo.png"
	writePNG(t, dir, name, 1000, 500, false)

	path, derivative := Ensure(dir, name, 320)
	if !derivative {
		t.Fatalf("expected a derivative, got the original (%s)", path)
	}
	if !strings.HasSuffix(path, "photo.320."+Version+".jpg") {
		t.Errorf("cache path = %q, want <base>.320.%s.jpg", path, Version)
	}
	img := decodeFile(t, path)
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 160 {
		t.Errorf("thumbnail size = %dx%d, want 320x160", b.Dx(), b.Dy())
	}
}

func TestEnsureNeverUpscales(t *testing.T) {
	dir := t.TempDir()
	name := "small.png"
	original := writePNG(t, dir, name, 200, 100, false)

	path, derivative := Ensure(dir, name, 640)
	if derivative {
		t.Fatalf("expected the original for a 200px source at w=640, got %s", path)
	}
	if path != original {
		t.Errorf("path = %q, want the original %q", path, original)
	}
	if _, err := os.Stat(filepath.Join(dir, "thumbs")); !os.IsNotExist(err) {
		t.Errorf("no cache directory should be created for a no-upscale request")
	}
}

func TestEnsureSkipsGIFAndNonImages(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"anim.gif", "doc.pdf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not really a file"), 0o644); err != nil {
			t.Fatal(err)
		}
		if path, derivative := Ensure(dir, name, 320); derivative || path != filepath.Join(dir, name) {
			t.Errorf("%s: got (%q, %v), want the original and false", name, path, derivative)
		}
	}
}

func TestEnsurePicksEncoderFromAlpha(t *testing.T) {
	dir := t.TempDir()

	opaque := "opaque.png"
	writePNG(t, dir, opaque, 800, 400, false)
	path, ok := Ensure(dir, opaque, 320)
	if !ok || !strings.HasSuffix(path, ".jpg") {
		t.Errorf("opaque source should cache as JPEG, got (%q, %v)", path, ok)
	}

	clear := "clear.png"
	writePNG(t, dir, clear, 800, 400, true)
	path, ok = Ensure(dir, clear, 320)
	if !ok || !strings.HasSuffix(path, ".png") {
		t.Errorf("transparent source should cache as PNG, got (%q, %v)", path, ok)
	}
}

func TestEnsureFallsBackOnCorruptAndMissing(t *testing.T) {
	dir := t.TempDir()

	corrupt := "corrupt.png"
	if err := os.WriteFile(filepath.Join(dir, corrupt), []byte("\x89PNG\r\n\x1a\nnot actually a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if path, derivative := Ensure(dir, corrupt, 320); derivative || path != filepath.Join(dir, corrupt) {
		t.Errorf("corrupt source: got (%q, %v), want the original and false", path, derivative)
	}

	if path, derivative := Ensure(dir, "missing.png", 320); derivative || path != filepath.Join(dir, "missing.png") {
		t.Errorf("missing source: got (%q, %v), want the original and false", path, derivative)
	}
}

func TestEnsureRejectsOversizedPixels(t *testing.T) {
	dir := t.TempDir()
	name := "huge.png"
	header := oversizedPNGHeader(100000, 100000)
	// The fabricated header must be readable by DecodeConfig; otherwise this
	// test would pass for the wrong reason (a decode error, not the pixel cap).
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(header)); err != nil {
		t.Fatalf("test fixture: DecodeConfig failed: %v", err)
	} else if cfg.Width != 100000 || cfg.Height != 100000 {
		t.Fatalf("test fixture: DecodeConfig = %dx%d, want 100000x100000", cfg.Width, cfg.Height)
	}
	if err := os.WriteFile(filepath.Join(dir, name), header, 0o644); err != nil {
		t.Fatal(err)
	}

	path, derivative := Ensure(dir, name, 320)
	if derivative {
		t.Fatalf("expected the original for a 10-gigapixel header, got %s", path)
	}
	if _, err := os.Stat(filepath.Join(dir, "thumbs")); !os.IsNotExist(err) {
		t.Errorf("no cache file should be written for an oversized source")
	}
}

func TestEnsureConcurrentSameKey(t *testing.T) {
	dir := t.TempDir()
	name := "racing.png"
	writePNG(t, dir, name, 1200, 600, false)

	var wg sync.WaitGroup
	results := make(chan bool, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, derivative := Ensure(dir, name, 320)
			results <- derivative
		}()
	}
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Errorf("every concurrent caller should end up with a derivative")
		}
	}

	matches, err := filepath.Glob(filepath.Join(dir, "thumbs", "racing.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("expected exactly one cache file, got %v", matches)
	}
	if tmp, _ := filepath.Glob(filepath.Join(dir, "thumbs", "tmp-*")); len(tmp) != 0 {
		t.Errorf("temp files left behind: %v", tmp)
	}
}

func TestRemoveDeletesAllWidths(t *testing.T) {
	dir := t.TempDir()
	name := "gone.png"
	writePNG(t, dir, name, 1600, 900, false)
	for _, w := range []int{320, 640} {
		if _, ok := Ensure(dir, name, w); !ok {
			t.Fatalf("failed to generate w=%d", w)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "thumbs", "gone.*")); len(matches) != 2 {
		t.Fatalf("expected 2 cached widths, got %v", matches)
	}

	Remove(dir, name)
	if matches, _ := filepath.Glob(filepath.Join(dir, "thumbs", "gone.*")); len(matches) != 0 {
		t.Errorf("Remove left files behind: %v", matches)
	}
}
