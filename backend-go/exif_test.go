package main

import (
	"bytes"
	"encoding/json"
	"image/jpeg"
	"math"
	"net/http/httptest"
	"os"
	"testing"
)

const cr3Fixture = "testdata/744A1255.CR3"

// TestCR3EmbeddedJPEGs logs every valid JPEG found inside the CR3 fixture and
// whether each one carries an Exif APP1 block. Run with -v to see the output.
// This is the reference diagnostic for understanding the CR3 container layout.
func TestCR3EmbeddedJPEGs(t *testing.T) {
	data, err := os.ReadFile(cr3Fixture)
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}

	type entry struct {
		offset  int
		width   int
		height  int
		exifLen int
	}
	var found []entry

	soi := []byte{0xFF, 0xD8, 0xFF}
	for off := 0; off+3 <= len(data); {
		idx := bytes.Index(data[off:], soi)
		if idx < 0 {
			break
		}
		start := off + idx
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data[start:]))
		if err == nil {
			tiff := extractJPEGExifBlock(data[start:])
			found = append(found, entry{
				offset:  start,
				width:   cfg.Width,
				height:  cfg.Height,
				exifLen: len(tiff),
			})
		}
		off = start + 2
	}

	if len(found) == 0 {
		t.Fatal("no valid JPEGs found in CR3 fixture")
	}
	for i, e := range found {
		t.Logf("JPEG[%d]: offset=%d  %dx%d  exifBytes=%d", i, e.offset, e.width, e.height, e.exifLen)
	}
}

// TestCR3TIFFBlocks finds standalone TIFF magic bytes ("II\x2a\x00" or "MM\x00\x2a")
// in the CR3 fixture. CR3 stores EXIF in ISOBMFF CMT1/CMT2 boxes as raw TIFF
// IFDs rather than wrapping them in a JPEG APP1 segment.
func TestCR3TIFFBlocks(t *testing.T) {
	data, err := os.ReadFile(cr3Fixture)
	if err != nil {
		t.Skipf("fixture not available: %v", err)
	}

	patterns := [][]byte{
		{0x49, 0x49, 0x2a, 0x00}, // "II\x2a\x00" little-endian TIFF
		{0x4d, 0x4d, 0x00, 0x2a}, // "MM\x00\x2a" big-endian TIFF
	}
	for _, p := range patterns {
		for off := 0; ; {
			idx := bytes.Index(data[off:], p)
			if idx < 0 {
				break
			}
			start := off + idx
			// Peek at the box name 8 bytes before (ISOBMFF: size[4] + type[4] + data)
			boxLabel := ""
			if start >= 8 {
				boxLabel = string(data[start-4 : start])
			}
			t.Logf("TIFF magic at offset=%d  box=%q", start, boxLabel)
			off = start + 4
		}
	}
}

func TestParsePhotoExifCR3(t *testing.T) {
	exif := parsePhotoExif(cr3Fixture)

	if exif.Make != "Canon" {
		t.Errorf("Make = %q, want %q", exif.Make, "Canon")
	}
	if exif.Model != "Canon EOS R6 Mark III" {
		t.Errorf("Model = %q, want %q", exif.Model, "Canon EOS R6 Mark III")
	}
	if exif.DateTaken != "2026:05:02 08:08:36" {
		t.Errorf("DateTaken = %q, want %q", exif.DateTaken, "2026:05:02 08:08:36")
	}
	if exif.ISO != 640 {
		t.Errorf("ISO = %d, want 640", exif.ISO)
	}

	// FNumber: exiftool reports 6.3 (stored as rational 63/10)
	if math.Abs(exif.FNumber-6.3) > 0.01 {
		t.Errorf("FNumber = %.4f, want ~6.3", exif.FNumber)
	}

	// ExposureTime: 1/1250
	if exif.ShutterNum == 0 || exif.ShutterDen == 0 {
		t.Fatalf("ShutterNum/Den not set (got %d/%d)", exif.ShutterNum, exif.ShutterDen)
	}
	ratio := float64(exif.ShutterNum) / float64(exif.ShutterDen)
	if math.Abs(ratio-1.0/1250) > 1e-6 {
		t.Errorf("ExposureTime = %d/%d (~%.6f), want ~1/1250", exif.ShutterNum, exif.ShutterDen, ratio)
	}

	// Dimensions from PixelXDimension / PixelYDimension tags
	if exif.Width != 6960 {
		t.Errorf("Width = %d, want 6960", exif.Width)
	}
	if exif.Height != 4640 {
		t.Errorf("Height = %d, want 4640", exif.Height)
	}
}

func TestPhotoInfoHandlerCR3(t *testing.T) {
	tmp := setupTestDirs(t)
	_ = tmp

	// Copy fixture into the test photo dir so resolvePhotoDir can find it.
	src := cr3Fixture
	dst := photoBaseDir + "/744A1255.CR3"
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatalf("writing fixture copy: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/photo-info?dir="+photoBaseDir+"&filename=744A1255.CR3", nil)
	w := httptest.NewRecorder()
	photoInfoHandler(w, req)

	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	check := func(key, want string) {
		t.Helper()
		got, _ := resp[key].(string)
		if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	check("camera_model", "Canon EOS R6 Mark III")
	check("f_number", "f/6.3")
	check("shutter_speed", "1/1250")
	check("iso", "640")
	check("megapixels", "32.3MP")

	if w, ok := resp["width"].(float64); !ok || int(w) != 6960 {
		t.Errorf("width = %v, want 6960", resp["width"])
	}
	if h, ok := resp["height"].(float64); !ok || int(h) != 4640 {
		t.Errorf("height = %v, want 4640", resp["height"])
	}
}

func TestExtractJPEGExifBlockRoundtrip(t *testing.T) {
	// Build a minimal JPEG with a fake APP1 Exif block and verify extraction.
	tiffPayload := []byte("II\x2a\x00\x08\x00\x00\x00\x00\x00") // minimal LE TIFF header + empty IFD
	app1Payload := append([]byte("Exif\x00\x00"), tiffPayload...)
	segLen := len(app1Payload) + 2 // includes the 2 length bytes
	blob := []byte{
		0xFF, 0xD8, // SOI
		0xFF, 0xE1, byte(segLen >> 8), byte(segLen), // APP1 marker + length
	}
	blob = append(blob, app1Payload...)

	got := extractJPEGExifBlock(blob)
	if string(got) != string(tiffPayload) {
		t.Errorf("extractJPEGExifBlock returned %q, want %q", got, tiffPayload)
	}
}

func TestExtractJPEGExifBlockNoExif(t *testing.T) {
	// JPEG with APP0 only — should return nil.
	app0 := []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
	segLen := len(app0) + 2
	blob := []byte{
		0xFF, 0xD8,
		0xFF, 0xE0, byte(segLen >> 8), byte(segLen),
	}
	blob = append(blob, app0...)

	if got := extractJPEGExifBlock(blob); got != nil {
		t.Errorf("expected nil for JPEG without Exif APP1, got %d bytes", len(got))
	}
}

func TestGCDUint32(t *testing.T) {
	cases := [][3]uint32{
		{1, 1250, 1},
		{10, 3200, 10},
		{6, 9, 3},
		{7, 13, 1},
		{0, 5, 5},
	}
	for _, c := range cases {
		if got := gcdUint32(c[0], c[1]); got != c[2] {
			t.Errorf("gcdUint32(%d,%d) = %d, want %d", c[0], c[1], got, c[2])
		}
	}
}
