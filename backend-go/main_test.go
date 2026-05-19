package main

import (
	"bytes"
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

// folderImportBody builds the JSON body for /api/import-from-folder.
func folderImportBody(sourceDir, destBase string) string {
	return `{"source_directory":"` + sourceDir + `","destination_base":"` + destBase + `","skip_duplicates":false}`
}

// TestFolderImportCustomDestination is the primary integration test for the bug fix:
// importing to a directory outside photoBaseDir must return an absolute new_directory
// and all subsequent API calls (getPhotos, servePhoto, serveThumbnail) must use the
// correct path.
func TestFolderImportCustomDestination(t *testing.T) {
	tmp := setupTestDirs(t)

	sourceDir := filepath.Join(tmp, "source")
	destBase := filepath.Join(tmp, "dest")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Put two test JPEGs in the source directory.
	createTestJPEG(t, filepath.Join(sourceDir, "IMG_001.JPG"))
	createTestJPEG(t, filepath.Join(sourceDir, "IMG_002.JPG"))

	// --- Step 1: import ---
	req := httptest.NewRequest(http.MethodPost, "/api/import-from-folder",
		strings.NewReader(folderImportBody(sourceDir, destBase)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	importFromFolderHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("import returned %d: %s", w.Code, w.Body.String())
	}

	var importResult map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&importResult); err != nil {
		t.Fatalf("import response not valid JSON: %v", err)
	}

	newDir, ok := importResult["new_directory"].(string)
	if !ok || newDir == "" {
		t.Fatalf("new_directory missing or empty in response: %v", importResult)
	}

	// new_directory must be an absolute path under destBase, not under photoBaseDir.
	if !filepath.IsAbs(newDir) {
		t.Errorf("new_directory should be absolute, got %q", newDir)
	}
	if !strings.HasPrefix(newDir, destBase) {
		t.Errorf("new_directory %q should be under destBase %q", newDir, destBase)
	}
	if strings.HasPrefix(newDir, photoBaseDir) {
		t.Errorf("new_directory %q must NOT be under photoBaseDir %q", newDir, photoBaseDir)
	}

	// Files must exist at the destination.
	for _, name := range []string{"IMG_001.JPG", "IMG_002.JPG"} {
		if _, err := os.Stat(filepath.Join(newDir, name)); os.IsNotExist(err) {
			t.Errorf("expected copied file %q to exist in %q", name, newDir)
		}
	}

	// --- Step 2: GET /api/photos?directory=<absolute path> ---
	req2 := httptest.NewRequest(http.MethodGet, "/api/photos?directory="+newDir, nil)
	w2 := httptest.NewRecorder()
	getPhotosHandler(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("getPhotos returned %d: %s", w2.Code, w2.Body.String())
	}
	var photos []string
	if err := json.NewDecoder(w2.Body).Decode(&photos); err != nil {
		t.Fatalf("getPhotos response not valid JSON: %v", err)
	}
	if len(photos) != 2 {
		t.Errorf("expected 2 photos, got %d: %v", len(photos), photos)
	}

	// --- Step 3: GET /photos/<filename>?dir=<absolute path> ---
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

	// --- Step 4: GET /thumbnail/<filename>?dir=<absolute path> ---
	req4 := httptest.NewRequest(http.MethodGet, "/thumbnail/IMG_001.JPG?dir="+newDir, nil)
	req4.URL.RawQuery = "dir=" + newDir
	w4 := httptest.NewRecorder()
	serveThumbnailHandler(w4, req4)

	if w4.Code != http.StatusOK {
		t.Errorf("serveThumbnail returned %d: %s", w4.Code, w4.Body.String())
	}

	// Thumbnail must be cached under thumbnailCacheDir using directoryCacheKey, NOT
	// under a path derived from photoBaseDir.
	cacheKey := directoryCacheKey(newDir)
	thumbPath := filepath.Join(thumbnailCacheDir, cacheKey, "IMG_001.JPG")
	if _, err := os.Stat(thumbPath); os.IsNotExist(err) {
		t.Errorf("thumbnail not cached at expected path %q", thumbPath)
	}
	// The old (wrong) path under photoBaseDir must not exist.
	wrongThumbPath := filepath.Join(thumbnailCacheDir, filepath.Base(newDir), "IMG_001.JPG")
	if cacheKey != filepath.Base(newDir) {
		if _, err := os.Stat(wrongThumbPath); err == nil {
			t.Errorf("thumbnail was cached at wrong path %q (should be at %q)", wrongThumbPath, thumbPath)
		}
	}
}

// TestFolderImportDefaultDestination verifies that the standard case (no custom destBase)
// is unchanged: new_directory is a basename inside photoBaseDir.
func TestFolderImportDefaultDestination(t *testing.T) {
	tmp := setupTestDirs(t)

	sourceDir := filepath.Join(tmp, "source")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	createTestJPEG(t, filepath.Join(sourceDir, "IMG_001.JPG"))

	// destination_base is empty → defaults to photoBaseDir.
	body := `{"source_directory":"` + sourceDir + `","destination_base":"","skip_duplicates":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/import-from-folder", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	importFromFolderHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("import returned %d: %s", w.Code, w.Body.String())
	}
	var result map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}

	newDir, _ := result["new_directory"].(string)
	if newDir == "" {
		t.Fatal("new_directory missing from response")
	}

	// In the default case new_directory is now the full absolute path under photoBaseDir.
	if !filepath.IsAbs(newDir) {
		t.Errorf("new_directory should be absolute, got %q", newDir)
	}
	if !strings.HasPrefix(newDir, photoBaseDir) {
		t.Errorf("new_directory %q should be under photoBaseDir %q", newDir, photoBaseDir)
	}

	// getPhotos must work with this path.
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
	if len(photos) != 1 {
		t.Errorf("expected 1 photo, got %d", len(photos))
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
