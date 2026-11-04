package thumbs

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

// tiffOrientation builds a minimal little/big-endian TIFF header whose IFD0
// carries (or omits) the Orientation tag.
func tiffOrientation(order binary.ByteOrder, tag, value uint16, withEntry bool) []byte {
	const ifdOff = 8
	entries := 1
	if !withEntry {
		entries = 0
	}
	buf := make([]byte, ifdOff+2+entries*12+4)
	if order == binary.LittleEndian {
		copy(buf[0:2], "II")
	} else {
		copy(buf[0:2], "MM")
	}
	order.PutUint16(buf[2:4], 42)
	order.PutUint32(buf[4:8], ifdOff)
	order.PutUint16(buf[ifdOff:ifdOff+2], uint16(entries))
	if entries == 1 {
		e := ifdOff + 2
		order.PutUint16(buf[e:e+2], tag)
		order.PutUint16(buf[e+2:e+4], 3) // SHORT
		order.PutUint32(buf[e+4:e+8], 1) // count
		order.PutUint16(buf[e+8:e+10], value)
	}
	return buf
}

func TestParseExifOrientation(t *testing.T) {
	tests := []struct {
		name string
		tiff []byte
		want int
	}{
		{name: "little endian 1", tiff: tiffOrientation(binary.LittleEndian, 0x0112, 1, true), want: 1},
		{name: "little endian 6", tiff: tiffOrientation(binary.LittleEndian, 0x0112, 6, true), want: 6},
		{name: "big endian 8", tiff: tiffOrientation(binary.BigEndian, 0x0112, 8, true), want: 8},
		{name: "no orientation tag", tiff: tiffOrientation(binary.LittleEndian, 0x010F, 6, true), want: 0},
		{name: "no entries", tiff: tiffOrientation(binary.LittleEndian, 0x0112, 6, false), want: 0},
		{name: "value out of range", tiff: tiffOrientation(binary.LittleEndian, 0x0112, 9, true), want: 0},
		{name: "bad byte order", tiff: append([]byte("XX"), make([]byte, 14)...), want: 0},
		{name: "bad magic", tiff: func() []byte {
			b := tiffOrientation(binary.LittleEndian, 0x0112, 6, true)
			b[2], b[3] = 43, 0
			return b
		}(), want: 0},
		{name: "truncated", tiff: tiffOrientation(binary.LittleEndian, 0x0112, 6, true)[:12], want: 0},
		{name: "ifd offset out of range", tiff: func() []byte {
			b := tiffOrientation(binary.LittleEndian, 0x0112, 6, true)
			binary.LittleEndian.PutUint32(b[4:8], 4096)
			return b
		}(), want: 0},
		{name: "too short", tiff: []byte("II"), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseExifOrientation(tt.tiff); got != tt.want {
				t.Errorf("parseExifOrientation() = %d, want %d", got, tt.want)
			}
		})
	}
}

// jpegWithOrientation builds just enough of a JPEG stream to exercise the
// segment walk: SOI, an APP0 that must be skipped, an APP1 carrying EXIF, EOI.
// No image data is present because the parser never reaches it.
func jpegWithOrientation(tiff []byte) []byte {
	var buf bytes.Buffer

	app0 := []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
	buf.Write([]byte{0xFF, 0xD8})
	buf.Write([]byte{0xFF, 0xE0})
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(app0)+2))
	buf.Write(l[:])
	buf.Write(app0)

	payload := append([]byte("Exif\x00\x00"), tiff...)
	buf.Write([]byte{0xFF, 0xE1})
	binary.BigEndian.PutUint16(l[:], uint16(len(payload)+2))
	buf.Write(l[:])
	buf.Write(payload)

	buf.Write([]byte{0xFF, 0xD9})
	return buf.Bytes()
}

func TestReadJPEGOrientation(t *testing.T) {
	reader := func(b []byte) *bytes.Reader { return bytes.NewReader(b) }

	tests := []struct {
		name string
		data []byte
		want int
	}{
		{
			name: "little endian 6",
			data: jpegWithOrientation(tiffOrientation(binary.LittleEndian, 0x0112, 6, true)),
			want: 6,
		},
		{
			name: "big endian 8",
			data: jpegWithOrientation(tiffOrientation(binary.BigEndian, 0x0112, 8, true)),
			want: 8,
		},
		{
			name: "no exif at all",
			data: []byte{0xFF, 0xD8, 0xFF, 0xD9},
			want: 1,
		},
		{
			name: "not a jpeg",
			data: []byte("GIF89a......"),
			want: 1,
		},
		{
			name: "app1 without exif header",
			data: append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x08}, []byte("XXXXXXXX")...),
			want: 1,
		},
		{
			name: "truncated segment",
			data: []byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x40, 'E', 'x'},
			want: 1,
		},
		{
			name: "fill bytes before marker",
			data: []byte{0xFF, 0xD8, 0xFF, 0xFF, 0xDA, 0x00},
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readJPEGOrientation(reader(tt.data)); got != tt.want {
				t.Errorf("readJPEGOrientation() = %d, want %d", got, tt.want)
			}
		})
	}
}

// testGrid is a 3x2 image whose red channel encodes x*10+y, so the expected
// matrix for each rotation is unambiguous.
func testGrid() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x*10 + y), A: 0xff})
		}
	}
	return img
}

func TestApplyOrientation(t *testing.T) {
	tests := []struct {
		orientation int
		want        [][]uint8
	}{
		{1, [][]uint8{{0, 10, 20}, {1, 11, 21}}},
		{2, [][]uint8{{20, 10, 0}, {21, 11, 1}}},
		{3, [][]uint8{{21, 11, 1}, {20, 10, 0}}},
		{4, [][]uint8{{1, 11, 21}, {0, 10, 20}}},
		{5, [][]uint8{{0, 1}, {10, 11}, {20, 21}}},
		{6, [][]uint8{{1, 0}, {11, 10}, {21, 20}}},
		{7, [][]uint8{{21, 20}, {11, 10}, {1, 0}}},
		{8, [][]uint8{{20, 21}, {10, 11}, {0, 1}}},
	}
	for _, tt := range tests {
		out := applyOrientation(testGrid(), tt.orientation)
		h := len(tt.want)
		w := len(tt.want[0])
		if b := out.Bounds(); b.Dx() != w || b.Dy() != h {
			t.Errorf("orientation %d: size = %dx%d, want %dx%d", tt.orientation, b.Dx(), b.Dy(), w, h)
			continue
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if got := out.NRGBAAt(x, y).R; got != tt.want[y][x] {
					t.Errorf("orientation %d: pixel (%d,%d) R = %d, want %d", tt.orientation, x, y, got, tt.want[y][x])
				}
			}
		}
	}
}

// TestApplyOrientationKeepsAlpha guards the PNG/JPEG encoder choice: the
// transform must not silently make transparent pixels opaque.
func TestApplyOrientationKeepsAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: 1, A: 0})
	img.SetNRGBA(1, 0, color.NRGBA{R: 2, A: 255})
	img.SetNRGBA(0, 1, color.NRGBA{R: 3, A: 255})
	img.SetNRGBA(1, 1, color.NRGBA{R: 4, A: 255})

	out := applyOrientation(img, 6)
	seen := map[uint8]uint8{}
	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := 0; x < out.Bounds().Dx(); x++ {
			c := out.NRGBAAt(x, y)
			seen[c.R] = c.A
		}
	}
	if seen[1] != 0 {
		t.Errorf("transparent pixel lost its alpha: %v", seen)
	}
	if seen[2] != 255 || seen[3] != 255 || seen[4] != 255 {
		t.Errorf("opaque pixels changed alpha: %v", seen)
	}
}
