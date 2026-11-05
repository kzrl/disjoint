// Package thumbs renders and caches downscaled derivatives of image
// attachments.
//
// Generation is lazy: the first request for a width decodes the original,
// applies EXIF orientation, scales it, and writes a JPEG (or PNG, when the
// pixels carry transparency) into <uploadDir>/thumbs. Storage paths are
// immutable UUIDs, so cached derivatives never need invalidating — only a
// change of algorithm does, which is what Version is for.
package thumbs

import (
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register the WebP decoder for image.Decode(Config)
)

const (
	// Version is part of every cache filename. Bump it whenever the resampler,
	// quality, or output rules change: old files become orphans (cleaned up
	// with their attachment, or by a future prune), and are never served again.
	Version = "v1"

	// DefaultWidth is used when the caller does not ask for a width.
	DefaultWidth = 640

	// MaxSourcePixels bounds the decoded pixel count. The check runs on
	// image.DecodeConfig (header only), so an oversized image is rejected
	// before its pixels are allocated.
	MaxSourcePixels = 40_000_000

	// maxConcurrent bounds simultaneous decode+scale work process-wide, which
	// bounds transient memory to roughly maxConcurrent decoded sources.
	maxConcurrent = 2

	jpegQuality = 82
)

// allowedWidths is the ?w= allowlist. It is deliberately small: every width is
// a cache key and a decode job, and three values cover the real surfaces
// (strip tiles, cards/wall tiles, wide retina cards).
var allowedWidths = map[int]struct{}{320: {}, 640: {}, 1280: {}}

// AllowedWidth reports whether w is a width the thumbnail handler may serve.
func AllowedWidth(w int) bool {
	_, ok := allowedWidths[w]
	return ok
}

// sem bounds concurrent generations across all requests.
var sem = make(chan struct{}, maxConcurrent)

// Ensure returns the file to serve and whether it is a cached derivative
// (true) or the original bytes (false).
//
// The original is returned for GIFs (animation is preserved rather than frozen
// into a first frame), for sources already narrower than the requested width
// (no upscaling), and for sources that cannot be decoded or exceed
// MaxSourcePixels. Serving something is always better than a broken <img>, so
// there is no error state.
func Ensure(uploadDir, storagePath string, width int) (path string, derivative bool) {
	name := filepath.Base(storagePath)
	original := filepath.Join(uploadDir, name)

	if width <= 0 {
		width = DefaultWidth
	}
	if !thumbnailable(name) {
		return original, false
	}
	if cached, ok := lookup(uploadDir, name, width); ok {
		return cached, true
	}

	// Bound decode work before touching the file: a slow or malicious image
	// cannot occupy more than maxConcurrent slots.
	sem <- struct{}{}
	defer func() { <-sem }()

	// Another request may have generated this width while we waited.
	if cached, ok := lookup(uploadDir, name, width); ok {
		return cached, true
	}

	f, err := os.Open(original)
	if err != nil {
		return original, false
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(name))
	orientation := 1
	if ext == ".jpg" || ext == ".jpeg" {
		orientation = readJPEGOrientation(f)
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return original, false
		}
	}

	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return original, false
	}
	// EXIF orientations 5-8 swap the axes, so the width the viewer sees is the
	// source's height.
	visualWidth := cfg.Width
	if orientation >= 5 {
		visualWidth = cfg.Height
	}
	if visualWidth <= width {
		return original, false
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxSourcePixels {
		return original, false
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return original, false
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return original, false
	}

	img := render(src, width, orientation)
	outExt := ".jpg"
	if hasAlpha(img) {
		outExt = ".png"
	}

	dir := filepath.Join(uploadDir, "thumbs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return original, false
	}
	tmp, err := os.CreateTemp(dir, "tmp-*")
	if err != nil {
		return original, false
	}
	tmpName := tmp.Name()
	if err := encode(tmp, img, outExt); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return original, false
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return original, false
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return original, false
	}
	// Rename is atomic, so a reader never observes a half-written thumbnail
	// and a crash leaves only a temp file (never a corrupt cache entry).
	final := filepath.Join(dir, cacheName(name, width, outExt))
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return original, false
	}
	return final, true
}

// Remove deletes every cached derivative for one attachment (all widths and
// versions). Called when a thread is purged, alongside the original's removal.
func Remove(uploadDir, storagePath string) {
	base := strings.TrimSuffix(filepath.Base(storagePath), filepath.Ext(storagePath))
	if base == "" || base == "." || base == ".." || strings.ContainsAny(base, `/\`) {
		return
	}
	matches, err := filepath.Glob(filepath.Join(uploadDir, "thumbs", base+".*"))
	if err != nil {
		return
	}
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// render scales src to the requested on-screen width and applies EXIF
// orientation. Orientation is applied to the *scaled* image (640 px wide, not
// 12 megapixels), so the pixel-wise transform stays cheap; the pre-rotation
// width is derived from the visually correct width so the output is exactly
// `width` wide either way.
func render(src image.Image, width, orientation int) *image.NRGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()

	preWidth := width
	if orientation >= 5 {
		// The final width comes from the source height: scale so the rotated
		// result is `width` wide.
		preWidth = int(math.Round(float64(sw) * float64(width) / float64(sh)))
		if preWidth < 1 {
			preWidth = 1
		}
	}
	return applyOrientation(scaleToWidth(src, preWidth), orientation)
}

// scaleToWidth scales src to the given width, preserving aspect ratio. Heights
// are rounded, never clamped to an aspect that would shift the image.
func scaleToWidth(src image.Image, width int) *image.NRGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if width < 1 {
		width = 1
	}
	if sw < 1 || sh < 1 {
		return image.NewNRGBA(image.Rect(0, 0, width, width))
	}
	height := int(math.Round(float64(sh) * float64(width) / float64(sw)))
	if height < 1 {
		height = 1
	}
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}

func hasAlpha(img *image.NRGBA) bool {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0xff {
			return true
		}
	}
	return false
}

func encode(w io.Writer, img image.Image, ext string) error {
	if ext == ".png" {
		return png.Encode(w, img)
	}
	return jpeg.Encode(w, img, &jpeg.Options{Quality: jpegQuality})
}

// cacheName keys a derivative on the source UUID, the width, and the renderer
// version: <uuid>.<width>.<version>.<ext>. The source extension is dropped so
// one attachment's derivatives share a single glob prefix for Remove.
func cacheName(storagePath string, width int, ext string) string {
	base := strings.TrimSuffix(filepath.Base(storagePath), filepath.Ext(storagePath))
	return fmt.Sprintf("%s.%d.%s%s", base, width, Version, ext)
}

func lookup(uploadDir, storagePath string, width int) (string, bool) {
	base := strings.TrimSuffix(filepath.Base(storagePath), filepath.Ext(storagePath))
	for _, ext := range []string{".jpg", ".png"} {
		p := filepath.Join(uploadDir, "thumbs", fmt.Sprintf("%s.%d.%s%s", base, width, Version, ext))
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

// IsImage reports whether name has an image extension the app serves (JPEG,
// PNG, WebP, GIF). GIFs are images but are never thumbnailed — Ensure hands
// back the original so animations survive — so callers use this to reject
// video/PDF paths, not to predict whether a derivative will be produced.
func IsImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif":
		return true
	}
	return false
}

func thumbnailable(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	}
	return false
}
