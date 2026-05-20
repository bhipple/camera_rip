package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsRawFile(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		{"P4061482.ORF", true},
		{"IMG_0001.CR3", true},
		{"IMG_0001.JPG", false},
		{"test.txt", false},
		{"ORF.JPG", false},
	}

	for _, tt := range tests {
		got := isRawFile(tt.filename)
		if got != tt.want {
			t.Errorf("isRawFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

// encodeTestJPEG returns a valid JPEG of the given dimensions.
func encodeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestScanExtractJPEGIgnoresTrailingSensorGarbage is the regression test for the
// CR3 thumbnail breakage: raw sensor data after the embedded JPEG contains
// random FF D9 byte pairs, and an EOI-finding strategy based on bytes.LastIndex
// would extend the "JPEG" into that garbage, producing a blob that jpeg.Decode
// rejects with "unknown marker" errors.
func TestScanExtractJPEGIgnoresTrailingSensorGarbage(t *testing.T) {
	embedded := encodeTestJPEG(t, 32, 32)

	var blob bytes.Buffer
	blob.WriteString("ftypcrx ") // pretend ISOBMFF header
	blob.Write(make([]byte, 64)) // non-JPEG container bytes
	blob.Write(embedded)         // the real embedded JPEG
	sensor := make([]byte, 8192) // simulated raw sensor data ...
	for i := 0; i+1 < len(sensor); i += 7 {
		sensor[i] = 0xFF
		sensor[i+1] = 0xD9 // ... with scattered FF D9 patterns
	}
	blob.Write(sensor)

	got, err := scanExtractJPEG(blob.Bytes(), "fake.cr3")
	if err != nil {
		t.Fatalf("scanExtractJPEG returned error: %v", err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("returned bytes did not decode as JPEG: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != 32 || b.Dy() != 32 {
		t.Errorf("decoded image bounds = %v, want 32x32", b)
	}
}

// TestScanExtractJPEGPicksLargest verifies that when multiple JPEGs are
// embedded (e.g., a small thumbnail and a larger preview), the largest is
// chosen.
func TestScanExtractJPEGPicksLargest(t *testing.T) {
	small := encodeTestJPEG(t, 16, 16)
	large := encodeTestJPEG(t, 64, 64)

	var blob bytes.Buffer
	blob.WriteString("ftypcrx ")
	blob.Write(small)
	blob.Write(make([]byte, 32))
	blob.Write(large)
	blob.Write(make([]byte, 1024))

	got, err := scanExtractJPEG(blob.Bytes(), "fake.cr3")
	if err != nil {
		t.Fatalf("scanExtractJPEG returned error: %v", err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("returned bytes did not decode as JPEG: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Errorf("expected largest (64x64), got %v", b)
	}
}

// TestScanExtractJPEGNoJPEG verifies the error path when no JPEG is present.
func TestScanExtractJPEGNoJPEG(t *testing.T) {
	blob := []byte("plain binary data with no SOI markers anywhere in it")
	if _, err := scanExtractJPEG(blob, "fake.cr3"); err == nil {
		t.Error("expected error for blob with no JPEG, got nil")
	}
}

// setupTestDirs creates temporary directories for photoBaseDir, thumbnailCacheDir, and
// rawPreviewCacheDir, overrides the package-level globals, and returns a cleanup function.
func setupTestDirs(t *testing.T) (tmpDir string) {
	t.Helper()
	tmp := t.TempDir()

	testPhotoBase := filepath.Join(tmp, "photos")
	testThumbnailCache := filepath.Join(testPhotoBase, ".thumbnails")
	testRawPreviewCache := filepath.Join(testPhotoBase, ".raw_previews")
	for _, d := range []string{testPhotoBase, testThumbnailCache, testRawPreviewCache} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	origPhotoBaseDir := photoBaseDir
	origThumbnailCacheDir := thumbnailCacheDir
	origRawPreviewCacheDir := rawPreviewCacheDir
	t.Cleanup(func() {
		photoBaseDir = origPhotoBaseDir
		thumbnailCacheDir = origThumbnailCacheDir
		rawPreviewCacheDir = origRawPreviewCacheDir
	})
	photoBaseDir = testPhotoBase
	thumbnailCacheDir = testThumbnailCache
	rawPreviewCacheDir = testRawPreviewCache

	return tmp
}

// createTestJPEG writes a minimal valid JPEG to path.
func createTestJPEG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
}

// TestFolderImportBrowsesSource verifies that import does not copy any files.
// new_directory is the source itself, and all API calls work against it directly.
func TestFolderImportBrowsesSource(t *testing.T) {
	tmp := setupTestDirs(t)

	sourceDir := filepath.Join(tmp, "source")
	destBase := filepath.Join(tmp, "dest")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	createTestJPEG(t, filepath.Join(sourceDir, "IMG_001.JPG"))
	createTestJPEG(t, filepath.Join(sourceDir, "IMG_002.JPG"))

	body := `{"source_directory":"` + sourceDir + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/import-from-folder", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	importFromFolderHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("import returned %d: %s", w.Code, w.Body.String())
	}
	var result map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("import response not valid JSON: %v", err)
	}

	newDir, ok := result["new_directory"].(string)
	if !ok || newDir == "" {
		t.Fatalf("new_directory missing or empty: %v", result)
	}

	// new_directory is the source itself — no files were copied.
	if newDir != sourceDir {
		t.Errorf("new_directory = %q, want source %q", newDir, sourceDir)
	}
	// destBase must not have been created.
	if _, err := os.Stat(destBase); err == nil {
		t.Errorf("destBase %q was created during import but should not have been", destBase)
	}

	// GET /api/photos works against the source.
	req2 := httptest.NewRequest(http.MethodGet, "/api/photos?directory="+newDir, nil)
	w2 := httptest.NewRecorder()
	getPhotosHandler(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("getPhotos returned %d: %s", w2.Code, w2.Body.String())
	}
	var photos []string
	if err := json.NewDecoder(w2.Body).Decode(&photos); err != nil {
		t.Fatal(err)
	}
	if len(photos) != 2 {
		t.Errorf("expected 2 photos, got %d: %v", len(photos), photos)
	}

	// GET /photos/<filename> works against the source.
	req3 := httptest.NewRequest(http.MethodGet, "/photos/IMG_001.JPG?dir="+newDir, nil)
	req3.URL.RawQuery = "dir=" + newDir
	w3 := httptest.NewRecorder()
	servePhotoHandler(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("servePhoto returned %d: %s", w3.Code, w3.Body.String())
	}
	if ct := w3.Header().Get("Content-Type"); !strings.Contains(ct, "image/") {
		t.Errorf("expected image Content-Type, got %q", ct)
	}

	// GET /thumbnail works against the source.
	req4 := httptest.NewRequest(http.MethodGet, "/thumbnail/IMG_001.JPG?dir="+newDir, nil)
	req4.URL.RawQuery = "dir=" + newDir
	w4 := httptest.NewRecorder()
	serveThumbnailHandler(w4, req4)
	if w4.Code != http.StatusOK {
		t.Errorf("serveThumbnail returned %d: %s", w4.Code, w4.Body.String())
	}
	cacheKey := directoryCacheKey(newDir)
	thumbPath := filepath.Join(thumbnailCacheDir, cacheKey, "IMG_001.JPG")
	if _, err := os.Stat(thumbPath); os.IsNotExist(err) {
		t.Errorf("thumbnail not cached at expected path %q", thumbPath)
	}
}

// TestSaveSelectedPhotos verifies that saving copies only the selected files from the
// source into destBase/YYYY-MM-DD/ subdirectories keyed by EXIF date (falling back to
// today's date when EXIF is absent).
func TestSaveSelectedPhotos(t *testing.T) {
	tmp := setupTestDirs(t)

	sourceDir := filepath.Join(tmp, "source")
	destBase := filepath.Join(tmp, "dest")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	createTestJPEG(t, filepath.Join(sourceDir, "IMG_001.JPG"))
	createTestJPEG(t, filepath.Join(sourceDir, "IMG_002.JPG"))
	createTestJPEG(t, filepath.Join(sourceDir, "IMG_003.JPG"))

	body, _ := json.Marshal(map[string]interface{}{
		"source_directory": sourceDir,
		"destination_base": destBase,
		"selected_files":   []string{"IMG_001.JPG", "IMG_003.JPG"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/save", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	saveSelectedPhotosHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", w.Code, w.Body.String())
	}
	var result map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("save response not valid JSON: %v", err)
	}

	newDir, ok := result["new_directory"].(string)
	if !ok || newDir == "" {
		t.Fatalf("new_directory missing from save response: %v", result)
	}

	// new_directory is now destBase itself; photos land in dated subdirs.
	if newDir != destBase {
		t.Errorf("new_directory = %q, want destBase %q", newDir, destBase)
	}

	// Test JPEGs have no EXIF, so the fallback date (today) is used.
	dateSubdir := filepath.Join(destBase, time.Now().Format("2006-01-02"))

	// Only the two selected files are present; IMG_002.JPG must not be copied.
	for _, name := range []string{"IMG_001.JPG", "IMG_003.JPG"} {
		if _, err := os.Stat(filepath.Join(dateSubdir, name)); os.IsNotExist(err) {
			t.Errorf("expected selected file %q in %q", name, dateSubdir)
		}
	}
	if _, err := os.Stat(filepath.Join(dateSubdir, "IMG_002.JPG")); err == nil {
		t.Errorf("IMG_002.JPG was copied but was not selected")
	}

	// getPhotos must work against the dated subdirectory.
	req2 := httptest.NewRequest(http.MethodGet, "/api/photos?directory="+dateSubdir, nil)
	w2 := httptest.NewRecorder()
	getPhotosHandler(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("getPhotos returned %d: %s", w2.Code, w2.Body.String())
	}
	var photos []string
	if err := json.NewDecoder(w2.Body).Decode(&photos); err != nil {
		t.Fatal(err)
	}
	if len(photos) != 2 {
		t.Errorf("expected 2 photos in destination, got %d: %v", len(photos), photos)
	}
}

// createJPEGWithEXIFDate writes a JPEG to path with DateTimeOriginal set to dateTaken
// (format "2006:01:02 15:04:05").
func createJPEGWithEXIFDate(t *testing.T, path, dateTaken string) {
	t.Helper()

	// Build a minimal little-endian TIFF block with one IFD entry: DateTimeOriginal (0x9003).
	dateBytes := append([]byte(dateTaken), 0x00) // null-terminated ASCII
	count := uint32(len(dateBytes))
	// Layout: 8-byte header + 2-byte IFD count + 12-byte entry + 4-byte next-IFD = 26, then string.
	stringOffset := uint32(26)

	var tiff bytes.Buffer
	tiff.WriteString("II")
	binary.Write(&tiff, binary.LittleEndian, uint16(0x002A))
	binary.Write(&tiff, binary.LittleEndian, uint32(8)) // IFD at offset 8
	binary.Write(&tiff, binary.LittleEndian, uint16(1)) // 1 entry
	binary.Write(&tiff, binary.LittleEndian, uint16(0x9003))
	binary.Write(&tiff, binary.LittleEndian, uint16(2)) // ASCII
	binary.Write(&tiff, binary.LittleEndian, count)
	binary.Write(&tiff, binary.LittleEndian, stringOffset)
	binary.Write(&tiff, binary.LittleEndian, uint32(0)) // next IFD = 0
	tiff.Write(dateBytes)

	tiffData := tiff.Bytes()
	app1Len := uint16(2 + 6 + len(tiffData)) // length includes 2-byte length field

	var app1 bytes.Buffer
	app1.WriteByte(0xFF)
	app1.WriteByte(0xE1)
	binary.Write(&app1, binary.BigEndian, app1Len)
	app1.WriteString("Exif\x00\x00")
	app1.Write(tiffData)

	// Encode a base JPEG and insert the APP1 segment immediately after SOI.
	baseJPEG := encodeTestJPEG(t, 4, 4)
	var out bytes.Buffer
	out.Write(baseJPEG[:2]) // FF D8 SOI
	out.Write(app1.Bytes())
	out.Write(baseJPEG[2:])

	if err := os.WriteFile(path, out.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestSaveSelectedPhotosExifDate verifies that each photo is exported to
// destBase/YYYY-MM-DD/ using the EXIF DateTimeOriginal date.
func TestSaveSelectedPhotosExifDate(t *testing.T) {
	tmp := setupTestDirs(t)

	sourceDir := filepath.Join(tmp, "source")
	destBase := filepath.Join(tmp, "dest")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Two photos with different EXIF dates; one with no EXIF (falls back to today).
	createJPEGWithEXIFDate(t, filepath.Join(sourceDir, "day1.JPG"), "2024:03:15 08:00:00")
	createJPEGWithEXIFDate(t, filepath.Join(sourceDir, "day2.JPG"), "2024:07:04 12:30:00")
	createTestJPEG(t, filepath.Join(sourceDir, "noexif.JPG"))

	body, _ := json.Marshal(map[string]interface{}{
		"source_directory": sourceDir,
		"destination_base": destBase,
		"selected_files":   []string{"day1.JPG", "day2.JPG", "noexif.JPG"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/save", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	saveSelectedPhotosHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", w.Code, w.Body.String())
	}

	// day1 → destBase/2024-03-15/
	if _, err := os.Stat(filepath.Join(destBase, "2024-03-15", "day1.JPG")); err != nil {
		t.Errorf("day1.JPG not found in 2024-03-15/: %v", err)
	}
	// day2 → destBase/2024-07-04/
	if _, err := os.Stat(filepath.Join(destBase, "2024-07-04", "day2.JPG")); err != nil {
		t.Errorf("day2.JPG not found in 2024-07-04/: %v", err)
	}
	// noexif → destBase/<today>/
	today := time.Now().Format("2006-01-02")
	if _, err := os.Stat(filepath.Join(destBase, today, "noexif.JPG")); err != nil {
		t.Errorf("noexif.JPG not found in %s/: %v", today, err)
	}
	// day1 must not bleed into day2's folder
	if _, err := os.Stat(filepath.Join(destBase, "2024-07-04", "day1.JPG")); err == nil {
		t.Errorf("day1.JPG should not be in 2024-07-04/")
	}
}

// TestResolvePhotoDir verifies the path resolution helper.
func TestResolvePhotoDir(t *testing.T) {
	tmp := setupTestDirs(t)
	_ = tmp

	// Relative path → joined with photoBaseDir.
	got := resolvePhotoDir("2026-01-01_12-00-00")
	want := filepath.Join(photoBaseDir, "2026-01-01_12-00-00")
	if got != want {
		t.Errorf("resolvePhotoDir(relative): got %q, want %q", got, want)
	}

	// Absolute path → returned as-is.
	abs := "/home/user/custom/2026-01-01_12-00-00"
	if got := resolvePhotoDir(abs); got != abs {
		t.Errorf("resolvePhotoDir(absolute): got %q, want %q", got, abs)
	}
}

// TestDirectoryCacheKey verifies the thumbnail cache key helper.
func TestDirectoryCacheKey(t *testing.T) {
	tmp := setupTestDirs(t)
	_ = tmp

	// Relative path → unchanged.
	rel := "2026-01-01_12-00-00"
	if got := directoryCacheKey(rel); got != rel {
		t.Errorf("directoryCacheKey(relative): got %q, want %q", got, rel)
	}

	// Absolute path under photoBaseDir → relative part only.
	underBase := filepath.Join(photoBaseDir, "2026-01-01_12-00-00")
	if got := directoryCacheKey(underBase); got != "2026-01-01_12-00-00" {
		t.Errorf("directoryCacheKey(under photoBaseDir): got %q, want %q", got, "2026-01-01_12-00-00")
	}

	// Absolute path outside photoBaseDir → leading slash stripped.
	outside := "/home/user/syncthing/canon_filtered/2026-01-01_12-00-00"
	want := "home/user/syncthing/canon_filtered/2026-01-01_12-00-00"
	if got := directoryCacheKey(outside); got != want {
		t.Errorf("directoryCacheKey(outside): got %q, want %q", got, want)
	}
}

// TestGetPhotosDateFilter verifies that the since/until query params filter the photo
// list by file mtime. This is the mechanism that makes From/To date filtering work
// when browsing a source directory directly (no copy-on-import).
func TestGetPhotosDateFilter(t *testing.T) {
	tmp := setupTestDirs(t)

	sourceDir := filepath.Join(tmp, "source")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create three photos and assign distinct mtimes.
	paths := map[string]time.Time{
		"old.JPG":    time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC),
		"middle.JPG": time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
		"new.JPG":    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	for name, mtime := range paths {
		p := filepath.Join(sourceDir, name)
		createTestJPEG(t, p)
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("Chtimes %s: %v", name, err)
		}
	}

	call := func(since, until string) []string {
		t.Helper()
		q := "directory=" + sourceDir
		if since != "" {
			q += "&since=" + since
		}
		if until != "" {
			q += "&until=" + until
		}
		req := httptest.NewRequest(http.MethodGet, "/api/photos?"+q, nil)
		w := httptest.NewRecorder()
		getPhotosHandler(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("getPhotos returned %d: %s", w.Code, w.Body.String())
		}
		var photos []string
		if err := json.NewDecoder(w.Body).Decode(&photos); err != nil {
			t.Fatal(err)
		}
		return photos
	}

	// No filter → all three photos.
	if got := call("", ""); len(got) != 3 {
		t.Errorf("no filter: expected 3 photos, got %v", got)
	}

	// since=2026-01-01 → middle and new only.
	got := call("2026-01-01", "")
	if len(got) != 2 {
		t.Errorf("since filter: expected 2 photos, got %v", got)
	}
	for _, name := range got {
		if name == "old.JPG" {
			t.Errorf("since filter: old.JPG should have been excluded")
		}
	}

	// since=2026-01-01 until=2026-04-01 → middle only.
	got = call("2026-01-01", "2026-04-01")
	if len(got) != 1 || got[0] != "middle.JPG" {
		t.Errorf("since+until filter: expected [middle.JPG], got %v", got)
	}

	// until=2025-01-01 → only old.JPG (the only file before 2025).
	got = call("", "2025-01-01")
	if len(got) != 1 || got[0] != "old.JPG" {
		t.Errorf("until filter: expected [old.JPG], got %v", got)
	}

	// until=2020-01-01 → nothing (all files are after 2020).
	got = call("", "2020-01-01")
	if len(got) != 0 {
		t.Errorf("until filter (excludes all): expected [], got %v", got)
	}
}
