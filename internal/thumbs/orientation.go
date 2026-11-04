package thumbs

import (
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"io"
)

// maxJPEGSegments bounds the marker scan so a pathological file cannot spin
// here. Real JPEGs have a handful of segments before SOS.
const maxJPEGSegments = 64

var errNoMarker = errors.New("thumbs: expected JPEG marker")

// readJPEGOrientation returns the EXIF orientation (1-8) of a JPEG stream, or 1
// when it is absent or malformed. Orientation is a display hint, never a
// correctness requirement, so every malformed input degrades to the identity
// rather than an error.
//
// It reads from the current position and consumes bytes; callers must seek back
// before decoding the image.
func readJPEGOrientation(r io.ReadSeeker) int {
	var soi [2]byte
	if _, err := io.ReadFull(r, soi[:]); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return 1
	}

	for i := 0; i < maxJPEGSegments; i++ {
		marker, err := nextJPEGMarker(r)
		if err != nil {
			return 1
		}
		// SOS starts entropy-coded data and EOI ends the file; an APP1 after
		// either is not something we look for.
		if marker == 0xDA || marker == 0xD9 {
			return 1
		}
		// Standalone markers carry no length payload.
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}

		var lengthBuf [2]byte
		if _, err := io.ReadFull(r, lengthBuf[:]); err != nil {
			return 1
		}
		length := int(binary.BigEndian.Uint16(lengthBuf[:]))
		if length < 2 {
			return 1
		}
		payload := length - 2

		// Only APP1 can carry EXIF; skip everything else.
		if marker != 0xE1 || payload < 6 {
			if _, err := r.Seek(int64(payload), io.SeekCurrent); err != nil {
				return 1
			}
			continue
		}

		// APP1 payloads are bounded by the 16-bit segment length, so this
		// allocation is at most ~64 KB.
		buf := make([]byte, payload)
		if _, err := io.ReadFull(r, buf); err != nil {
			return 1
		}
		if string(buf[:6]) == "Exif\x00\x00" {
			if o := parseExifOrientation(buf[6:]); o != 0 {
				return o
			}
		}
	}
	return 1
}

// nextJPEGMarker consumes bytes up to and including the next marker code,
// skipping 0xFF fill bytes.
func nextJPEGMarker(r io.Reader) (byte, error) {
	var b [1]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		if b[0] != 0xFF {
			return 0, errNoMarker
		}
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		switch b[0] {
		case 0xFF: // fill byte, keep looking for the marker code
			continue
		case 0x00: // escaped 0xFF inside entropy data, not a marker
			return 0, errNoMarker
		}
		return b[0], nil
	}
}

// parseExifOrientation parses the TIFF header that follows "Exif\x00\x00" and
// returns the Orientation tag (0x0112) when present and valid, else 0.
func parseExifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}

	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 0
	}

	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 0
	}
	count := int(order.Uint16(tiff[ifd : ifd+2]))
	if ifd+2+count*12 > len(tiff) {
		return 0
	}

	const orientationTag = 0x0112
	for i := 0; i < count; i++ {
		e := ifd + 2 + i*12
		if order.Uint16(tiff[e:e+2]) != orientationTag {
			continue
		}
		// Type 3 is SHORT; a single value is stored inline in the entry.
		if order.Uint16(tiff[e+2:e+4]) != 3 || order.Uint32(tiff[e+4:e+8]) < 1 {
			return 0
		}
		v := int(order.Uint16(tiff[e+8 : e+10]))
		if v < 1 || v > 8 {
			return 0
		}
		return v
	}
	return 0
}

// applyOrientation returns img transformed per the EXIF orientation value
// (1-8). Values outside that range — and the identity — return the pixels
// unchanged.
//
// The mapping is the standard one: 2 mirrors horizontally, 3 rotates 180°,
// 4 mirrors vertically, 5 transposes, 6 rotates 90° clockwise, 7 is the
// transverse flip, 8 rotates 270° clockwise. Cases 5-8 swap the dimensions.
func applyOrientation(img image.Image, orientation int) *image.NRGBA {
	if orientation < 2 || orientation > 8 {
		if n, ok := img.(*image.NRGBA); ok {
			return n
		}
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if orientation >= 5 && orientation <= 8 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))

	// The renderer always scales into an NRGBA, so the fast path is the only
	// one in practice; the fallback keeps the function usable on any image.
	src, _ := img.(*image.NRGBA)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch orientation {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			default:
				dx, dy = x, y
			}

			var c color.NRGBA
			if src != nil {
				i := src.PixOffset(b.Min.X+x, b.Min.Y+y)
				c = color.NRGBA{R: src.Pix[i], G: src.Pix[i+1], B: src.Pix[i+2], A: src.Pix[i+3]}
			} else {
				r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				c = color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bl >> 8), A: uint8(a >> 8)}
			}

			j := dst.PixOffset(dx, dy)
			dst.Pix[j] = c.R
			dst.Pix[j+1] = c.G
			dst.Pix[j+2] = c.B
			dst.Pix[j+3] = c.A
		}
	}
	return dst
}
