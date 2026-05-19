package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nfnt/resize"
)

var (
	photoBaseDir       string
	thumbnailCacheDir  string
	rawPreviewCacheDir string
	thumbnailSize      = 200
)

var rawExtensions = []string{".cr3", ".orf"}

func isRawFile(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range rawExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// extractEmbeddedJPEG returns the embedded JPEG preview bytes from a RAW file.
//
// Strategy:
//   - TIFF-based RAWs (ORF, etc.): parse TIFF IFDs and use tags 0x0201/0x0202
//     (JPEGInterchangeFormat / Length) for exact offset and length. This avoids
//     including raw sensor data that follows the JPEG in the file.
//   - ISOBMFF-based RAWs (CR3, etc.): scan for all JPEG SOI markers and return
//     the largest segment bounded by the next SOI (or EOF).
func extractEmbeddedJPEG(rawPath string) ([]byte, error) {
	data, err := os.ReadFile(rawPath)
	if err != nil {
		return nil, err
	}
	if len(data) < 8 {
		return nil, fmt.Errorf("file too small: %s", rawPath)
	}

	// TIFF magic: "II" (little-endian) or "MM" (big-endian)
	if (data[0] == 'I' && data[1] == 'I') || (data[0] == 'M' && data[1] == 'M') {
		if j, err := tiffExtractJPEG(data); err == nil {
			return j, nil
		}
	}

	return scanExtractJPEG(data, rawPath)
}

// tiffExtractJPEG walks TIFF IFD chains looking for tags 0x0201/0x0202 that
// point to an embedded JPEG preview (standard in EXIF / Olympus ORF IFD1).
func tiffExtractJPEG(data []byte) ([]byte, error) {
	var bo binary.ByteOrder
	if data[0] == 'I' {
		bo = binary.LittleEndian
	} else {
		bo = binary.BigEndian
	}
	if bo.Uint16(data[2:]) != 42 {
		return nil, fmt.Errorf("not a TIFF file")
	}

	ifdOff := bo.Uint32(data[4:])
	for ifdOff != 0 && int(ifdOff)+2 <= len(data) {
		n := int(bo.Uint16(data[ifdOff:]))
		base := int(ifdOff) + 2
		var jpegOff, jpegLen uint32
		for i := 0; i < n; i++ {
			e := base + i*12
			if e+12 > len(data) {
				break
			}
			tag := bo.Uint16(data[e:])
			val := bo.Uint32(data[e+8:])
			switch tag {
			case 0x0201:
				jpegOff = val
			case 0x0202:
				jpegLen = val
			}
		}
		if jpegOff > 0 && jpegLen > 0 && int(jpegOff)+int(jpegLen) <= len(data) {
			return data[jpegOff : jpegOff+jpegLen], nil
		}
		// Follow linked-list to next IFD
		nextOff := base + n*12
		if nextOff+4 > len(data) {
			break
		}
		ifdOff = bo.Uint32(data[nextOff:])
	}
	return nil, fmt.Errorf("JPEG offset/length tags not found in TIFF IFDs")
}

// scanExtractJPEG finds the largest embedded JPEG by trying each FF D8 FF
// candidate as a JPEG header. Used for ISOBMFF-based RAWs (CR3) where TIFF
// parsing doesn't apply.
//
// We can't bound segments by locating EOI ourselves: CR3 raw sensor data
// contains random FF D9 byte pairs that look like EOI markers, so any
// byte-scan approach produces a JPEG with trailing garbage that fails to
// decode. Instead, validate each SOI with jpeg.DecodeConfig (cheap — header
// only) and return data[start:]. The caller's jpeg.Decode stops at the real
// EOI and ignores trailing bytes.
func scanExtractJPEG(data []byte, rawPath string) ([]byte, error) {
	soi := []byte{0xFF, 0xD8, 0xFF}
	var bestStart = -1
	var bestArea int

	for off := 0; off+3 <= len(data); {
		idx := bytes.Index(data[off:], soi)
		if idx < 0 {
			break
		}
		start := off + idx
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(data[start:])); err == nil {
			area := cfg.Width * cfg.Height
			if area > bestArea {
				bestArea = area
				bestStart = start
			}
		}
		off = start + 2
	}

	if bestStart < 0 {
		return nil, fmt.Errorf("no embedded JPEG found in %s", rawPath)
	}
	return data[bestStart:], nil
}

type spaFileSystem struct {
	root http.FileSystem
}

func (fs *spaFileSystem) Open(name string) (http.File, error) {
	f, err := fs.root.Open(name)
	if os.IsNotExist(err) {
		return fs.root.Open("index.html")
	}
	return f, err
}

func main() {
	devMode := flag.Bool("dev", false, "Run in development mode (do not serve static files)")
	flag.Parse()

	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get user home directory: %v", err)
	}
	photoBaseDir = filepath.Join(userHomeDir, "Pictures", "photos")
	thumbnailCacheDir = filepath.Join(photoBaseDir, ".thumbnails")
	rawPreviewCacheDir = filepath.Join(photoBaseDir, ".raw_previews")

	if err := os.MkdirAll(photoBaseDir, 0755); err != nil {
		log.Fatalf("Failed to create photo base directory: %v", err)
	}
	if err := os.MkdirAll(thumbnailCacheDir, 0755); err != nil {
		log.Fatalf("Failed to create thumbnail cache directory: %v", err)
	}
	if err := os.MkdirAll(rawPreviewCacheDir, 0755); err != nil {
		log.Fatalf("Failed to create raw preview cache directory: %v", err)
	}

	http.HandleFunc("/api/directories", corsHandler(listDirectoriesHandler))
	http.HandleFunc("/api/photos", corsHandler(getPhotosHandler))
	http.HandleFunc("/api/save", corsHandler(saveSelectedPhotosHandler))
	http.HandleFunc("/api/import-from-folder", corsHandler(importFromFolderHandler))
	http.HandleFunc("/api/import-from-folder-preview", corsHandler(importFromFolderPreviewHandler))
	http.HandleFunc("/api/recent-paths", corsHandler(recentPathsHandler))
	http.HandleFunc("/api/selected-photos", corsHandler(getSelectedPhotosHandler))
	http.HandleFunc("/api/delete-photos", corsHandler(deletePhotosHandler))
	http.HandleFunc("/photos/", corsHandler(servePhotoHandler))
	http.HandleFunc("/thumbnail/", corsHandler(serveThumbnailHandler))

	// Serve the frontend only if not in dev mode
	if !*devMode {
		fs, err := fs.Sub(frontend, "frontend/build")
		if err != nil {
			log.Fatalf("Failed to create sub file system: %v", err)
		}
		http.Handle("/", http.FileServer(&spaFileSystem{http.FS(fs)}))
	} else {
		log.Println("Running in dev mode. Frontend not served at root. Access via localhost:3000")
	}

	log.Println("Starting server on :5001")
	if err := http.ListenAndServe(":5001", nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func corsHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		h(w, r)
	}
}

func listDirectoriesHandler(w http.ResponseWriter, r *http.Request) {
	files, err := ioutil.ReadDir(photoBaseDir)
	if err != nil {
		http.Error(w, "Failed to read photo base directory", http.StatusInternalServerError)
		return
	}

	var dirs []string
	for _, file := range files {
		if file.IsDir() && file.Name() != ".thumbnails" {
			dirs = append(dirs, file.Name())
		}
	}

	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dirs)
}

// resolvePhotoDir returns the absolute path for a directory: if already absolute, use as-is;
// otherwise join with photoBaseDir.
func resolvePhotoDir(directory string) string {
	if filepath.IsAbs(directory) {
		return directory
	}
	return filepath.Join(photoBaseDir, directory)
}

// directoryCacheKey returns a safe relative path to use under cache directories.
// Paths already under photoBaseDir become relative; other absolute paths strip the leading slash.
func directoryCacheKey(directory string) string {
	if filepath.IsAbs(directory) {
		rel, err := filepath.Rel(photoBaseDir, directory)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
		return strings.TrimPrefix(directory, "/")
	}
	return directory
}

func getPhotosHandler(w http.ResponseWriter, r *http.Request) {
	directory := r.URL.Query().Get("directory")
	if directory == "" {
		http.Error(w, "Missing 'directory' query parameter", http.StatusBadRequest)
		return
	}

	targetDir := resolvePhotoDir(directory)
	files, err := ioutil.ReadDir(targetDir)
	if err != nil {
		http.Error(w, "Failed to read photo directory", http.StatusInternalServerError)
		return
	}

	var photos []string
	for _, file := range files {
		if !file.IsDir() && !strings.HasPrefix(file.Name(), "._") {
			lowerName := strings.ToLower(file.Name())
			isImg := strings.HasSuffix(lowerName, ".png") || strings.HasSuffix(lowerName, ".jpg") ||
				strings.HasSuffix(lowerName, ".jpeg") || strings.HasSuffix(lowerName, ".gif")
			if isImg || isRawFile(file.Name()) {
				photos = append(photos, file.Name())
			}
		}
	}

	sort.Strings(photos)

	// Start async thumbnail generation for this directory
	if len(photos) > 0 {
		go func() {
			log.Printf("Starting background thumbnail generation for directory: %s (%d photos)", directory, len(photos))
			preGenerateThumbnails(directory, photos)
		}()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(photos)
}

func getSelectedPhotosHandler(w http.ResponseWriter, r *http.Request) {
	directory := r.URL.Query().Get("directory")
	if directory == "" {
		http.Error(w, "Missing 'directory' query parameter", http.StatusBadRequest)
		return
	}

	selectedDir := filepath.Join(resolvePhotoDir(directory), "selected")
	files, err := ioutil.ReadDir(selectedDir)
	if err != nil {
		// If the directory doesn't exist, it just means no photos have been selected yet.
		// Return an empty list.
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]string{})
			return
		}
		http.Error(w, "Failed to read selected photo directory", http.StatusInternalServerError)
		return
	}

	var photos []string
	for _, file := range files {
		if !file.IsDir() && !strings.HasPrefix(file.Name(), "._") {
			lowerName := strings.ToLower(file.Name())
			isImg := strings.HasSuffix(lowerName, ".png") || strings.HasSuffix(lowerName, ".jpg") ||
				strings.HasSuffix(lowerName, ".jpeg") || strings.HasSuffix(lowerName, ".gif")
			if isImg || isRawFile(file.Name()) {
				photos = append(photos, file.Name())
			}
		}
	}

	sort.Strings(photos)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(photos)
}

func saveSelectedPhotosHandler(w http.ResponseWriter, r *http.Request) {
	var data struct {
		SelectedFiles []string `json:"selected_files"`
		Directory     string   `json:"directory"`
	}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(data.SelectedFiles) == 0 || data.Directory == "" {
		http.Error(w, "Missing 'selected_files' or 'directory' in request", http.StatusBadRequest)
		return
	}

	sourceDir := resolvePhotoDir(data.Directory)
	destinationDir := filepath.Join(sourceDir, "selected")

	if err := os.MkdirAll(destinationDir, 0755); err != nil {
		http.Error(w, "Failed to create destination directory", http.StatusInternalServerError)
		return
	}

	for _, filename := range data.SelectedFiles {
		sourcePath := filepath.Join(sourceDir, filename)
		destinationPath := filepath.Join(destinationDir, filename)

		sourceFile, err := os.Open(sourcePath)
		if err != nil {
			log.Printf("Failed to open source file: %v", err)
			continue
		}
		defer sourceFile.Close()

		destinationFile, err := os.Create(destinationPath)
		if err != nil {
			log.Printf("Failed to create destination file: %v", err)
			continue
		}
		defer destinationFile.Close()

		if _, err := io.Copy(destinationFile, sourceFile); err != nil {
			log.Printf("Failed to copy file: %v", err)
			continue
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": "Successfully copied " + strconv.Itoa(len(data.SelectedFiles)) + " files to '" + destinationDir + "'",
	})
}

func buildImportedFilesSet() map[string]bool {
	importedFiles := make(map[string]bool)

	dirs, err := ioutil.ReadDir(photoBaseDir)
	if err != nil {
		return importedFiles
	}

	for _, dir := range dirs {
		if dir.IsDir() && dir.Name() != ".thumbnails" {
			dirPath := filepath.Join(photoBaseDir, dir.Name())
			files, err := ioutil.ReadDir(dirPath)
			if err != nil {
				continue
			}

			for _, file := range files {
				if !file.IsDir() {
					importedFiles[file.Name()] = true
				}
			}
		}
	}

	return importedFiles
}

func deletePhotosHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var data struct {
		Directory string   `json:"directory"`
		Files     []string `json:"files"`
	}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if data.Directory == "" {
		http.Error(w, "Missing 'directory' in request", http.StatusBadRequest)
		return
	}

	if len(data.Files) == 0 {
		http.Error(w, "No files specified for deletion", http.StatusBadRequest)
		return
	}

	targetDir := resolvePhotoDir(data.Directory)
	deletedCount := 0
	notFoundCount := 0
	errorCount := 0

	for _, filename := range data.Files {
		// Security: ensure filename doesn't contain path traversal
		if strings.Contains(filename, "..") || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
			log.Printf("Skipping invalid filename: %s", filename)
			errorCount++
			continue
		}

		filePath := filepath.Join(targetDir, filename)

		// Verify the file is actually in the target directory
		absPath, err := filepath.Abs(filePath)
		if err != nil {
			log.Printf("Failed to get absolute path for %s: %v", filePath, err)
			errorCount++
			continue
		}

		absTargetDir, err := filepath.Abs(targetDir)
		if err != nil {
			log.Printf("Failed to get absolute path for target dir: %v", err)
			errorCount++
			continue
		}

		if !strings.HasPrefix(absPath, absTargetDir) {
			log.Printf("Security check failed: file path %s is outside target directory", absPath)
			errorCount++
			continue
		}

		if err := os.Remove(filePath); err != nil {
			if os.IsNotExist(err) {
				notFoundCount++
			} else {
				log.Printf("Failed to delete file %s: %v", filePath, err)
				errorCount++
			}
		} else {
			deletedCount++
			log.Printf("Deleted file: %s", filename)

			// Also try to delete thumbnail if it exists
			thumbnailPath := filepath.Join(thumbnailCacheDir, directoryCacheKey(data.Directory), filename)
			if err := os.Remove(thumbnailPath); err != nil && !os.IsNotExist(err) {
				log.Printf("Failed to delete thumbnail %s: %v", thumbnailPath, err)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":   "Delete operation complete",
		"deleted":   deletedCount,
		"not_found": notFoundCount,
		"errors":    errorCount,
	})
}

type recentPathsData struct {
	Source      []string `json:"source"`
	Destination []string `json:"destination"`
}

func getRecentPathsFilePath() string {
	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		log.Printf("Failed to get user home directory: %v", err)
		return ""
	}
	cacheDir := filepath.Join(userHomeDir, ".cache", "camera_rip")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		log.Printf("Failed to create cache directory: %v", err)
		return ""
	}
	return filepath.Join(cacheDir, "recent_paths.json")
}

func loadRecentPaths() recentPathsData {
	var paths recentPathsData
	filePath := getRecentPathsFilePath()
	if filePath == "" {
		return paths
	}

	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Failed to read recent paths file: %v", err)
		}
		return paths
	}

	if err := json.Unmarshal(data, &paths); err != nil {
		log.Printf("Failed to parse recent paths file: %v", err)
		return recentPathsData{}
	}

	return paths
}

func saveRecentPath(path, pathType string) error {
	filePath := getRecentPathsFilePath()
	if filePath == "" {
		return nil
	}

	paths := loadRecentPaths()

	var targetList *[]string
	if pathType == "source" {
		targetList = &paths.Source
	} else if pathType == "destination" {
		targetList = &paths.Destination
	} else {
		return nil
	}

	// Remove if already exists
	for i, p := range *targetList {
		if p == path {
			*targetList = append((*targetList)[:i], (*targetList)[i+1:]...)
			break
		}
	}

	// Add to front
	*targetList = append([]string{path}, *targetList...)

	// Keep only last 10
	if len(*targetList) > 10 {
		*targetList = (*targetList)[:10]
	}

	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile(filePath, data, 0644)
}

func recentPathsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		pathType := r.URL.Query().Get("type")
		if pathType != "source" && pathType != "destination" {
			http.Error(w, "Invalid 'type' parameter. Must be 'source' or 'destination'", http.StatusBadRequest)
			return
		}

		paths := loadRecentPaths()
		var result []string
		if pathType == "source" {
			result = paths.Source
		} else {
			result = paths.Destination
		}

		if result == nil {
			result = []string{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	} else if r.Method == "POST" {
		var data struct {
			Path string `json:"path"`
			Type string `json:"type"`
		}

		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if data.Path == "" || data.Type == "" {
			http.Error(w, "Missing 'path' or 'type' in request", http.StatusBadRequest)
			return
		}

		if data.Type != "source" && data.Type != "destination" {
			http.Error(w, "Invalid 'type'. Must be 'source' or 'destination'", http.StatusBadRequest)
			return
		}

		if err := saveRecentPath(data.Path, data.Type); err != nil {
			http.Error(w, "Failed to save recent path", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"message": "Recent path saved",
		})
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func importFromFolderPreviewHandler(w http.ResponseWriter, r *http.Request) {
	var data struct {
		SourceDirectory string `json:"source_directory"`
		DestinationBase string `json:"destination_base"`
		Since           string `json:"since"`
		Until           string `json:"until"`
		SkipDuplicates  bool   `json:"skip_duplicates"`
		TargetDirectory string `json:"target_directory"`
		ImportVideos    bool   `json:"import_videos"`
	}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil && err != io.EOF {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if data.SourceDirectory == "" {
		http.Error(w, "Missing 'source_directory'", http.StatusBadRequest)
		return
	}

	// Validate source directory exists
	if _, err := os.Stat(data.SourceDirectory); os.IsNotExist(err) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_files":     0,
			"files_to_import": 0,
			"files_to_skip":   0,
			"error":           "Source directory does not exist",
		})
		return
	}

	// Parse dates
	var sinceDate time.Time
	var untilDate time.Time
	var err error
	if data.Since != "" {
		sinceDate, err = time.Parse("2006-01-02", data.Since)
		if err != nil {
			http.Error(w, "Invalid date format. Please use YYYY-MM-DD.", http.StatusBadRequest)
			return
		}
	}
	if data.Until != "" {
		untilDate, err = time.Parse("2006-01-02", data.Until)
		if err != nil {
			http.Error(w, "Invalid date format. Please use YYYY-MM-DD.", http.StatusBadRequest)
			return
		}
		untilDate = untilDate.AddDate(0, 0, 1)
	}

	// Scan directory for files
	var allFiles []os.FileInfo
	filepath.Walk(data.SourceDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && !strings.HasPrefix(info.Name(), "._") {
			allFiles = append(allFiles, info)
		}
		return nil
	})

	// Build imported files set if skip duplicates enabled
	var importedFiles map[string]bool
	if data.SkipDuplicates {
		importedFiles = buildImportedFilesSet()
	}

	// Determine destination directory for duplicate checking
	var destinationDir string
	if data.TargetDirectory != "" {
		destinationDir = filepath.Join(photoBaseDir, data.TargetDirectory)
		if _, err := os.Stat(destinationDir); os.IsNotExist(err) {
			http.Error(w, "Target directory does not exist", http.StatusBadRequest)
			return
		}
	}

	totalFiles := 0
	filesToImport := 0
	skippedDuplicates := 0
	skippedByDate := 0
	skippedVideos := 0
	dailyBreakdown := make(map[string]int)

	for _, file := range allFiles {
		lowerName := strings.ToLower(file.Name())
		isImage := strings.HasSuffix(lowerName, ".jpg") || strings.HasSuffix(lowerName, ".jpeg") ||
			strings.HasSuffix(lowerName, ".png") || strings.HasSuffix(lowerName, ".gif")
		isMp4 := strings.HasSuffix(lowerName, ".mp4")
		isRaw := strings.HasSuffix(lowerName, ".cr3") || strings.HasSuffix(lowerName, ".cr2") ||
			strings.HasSuffix(lowerName, ".arw") || strings.HasSuffix(lowerName, ".nef") ||
			strings.HasSuffix(lowerName, ".raf") || strings.HasSuffix(lowerName, ".dng")

		if isImage || isMp4 || isRaw {
			totalFiles++
		}

		if !isImage && !isRaw && (!isMp4 || !data.ImportVideos) {
			if isMp4 {
				skippedVideos++
			}
			continue
		}

		// Check date filter
		modTime := file.ModTime()
		if !sinceDate.IsZero() && modTime.Before(sinceDate) {
			skippedByDate++
			continue
		}
		if !untilDate.IsZero() && !modTime.Before(untilDate) {
			skippedByDate++
			continue
		}

		// Check duplicates
		if data.SkipDuplicates && importedFiles[file.Name()] {
			skippedDuplicates++
			continue
		}

		// Check if exists in target destination
		if destinationDir != "" {
			destinationFile := filepath.Join(destinationDir, file.Name())
			if _, err := os.Stat(destinationFile); err == nil {
				skippedDuplicates++
				continue
			}
		}

		filesToImport++
		dateKey := modTime.Format("2006-01-02")
		dailyBreakdown[dateKey]++
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"total_files":        totalFiles,
		"files_to_import":    filesToImport,
		"skipped_duplicates": skippedDuplicates,
		"skipped_by_date":    skippedByDate,
		"skipped_videos":     skippedVideos,
		"daily_breakdown":    dailyBreakdown,
	})
}

func importFromFolderHandler(w http.ResponseWriter, r *http.Request) {
	var data struct {
		SourceDirectory string `json:"source_directory"`
		DestinationBase string `json:"destination_base"`
		Since           string `json:"since"`
		Until           string `json:"until"`
		SkipDuplicates  bool   `json:"skip_duplicates"`
		TargetDirectory string `json:"target_directory"`
		ImportVideos    bool   `json:"import_videos"`
	}

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil && err != io.EOF {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if data.SourceDirectory == "" {
		http.Error(w, "Missing 'source_directory'", http.StatusBadRequest)
		return
	}

	// Validate source directory exists
	if _, err := os.Stat(data.SourceDirectory); os.IsNotExist(err) {
		http.Error(w, "Source directory does not exist", http.StatusNotFound)
		return
	}

	// Determine destination directory
	var destinationDir string
	var isNewBatch bool
	var destBase string

	if data.DestinationBase != "" {
		destBase = data.DestinationBase
	} else {
		destBase = photoBaseDir
	}

	// Expand tilde in destination base
	if strings.HasPrefix(destBase, "~/") {
		userHomeDir, err := os.UserHomeDir()
		if err != nil {
			http.Error(w, "Failed to get user home directory", http.StatusInternalServerError)
			return
		}
		destBase = filepath.Join(userHomeDir, destBase[2:])
	}

	if data.TargetDirectory != "" {
		destinationDir = filepath.Join(destBase, data.TargetDirectory)
		isNewBatch = false
		if _, err := os.Stat(destinationDir); os.IsNotExist(err) {
			http.Error(w, "Target directory does not exist", http.StatusBadRequest)
			return
		}
	} else {
		destinationDir = filepath.Join(destBase, time.Now().Format("2006-01-02_15-04-05"))
		isNewBatch = true
	}

	// Parse dates
	var sinceDate time.Time
	var untilDate time.Time
	var err error
	if data.Since != "" {
		sinceDate, err = time.Parse("2006-01-02", data.Since)
		if err != nil {
			http.Error(w, "Invalid date format. Please use YYYY-MM-DD.", http.StatusBadRequest)
			return
		}
	}
	if data.Until != "" {
		untilDate, err = time.Parse("2006-01-02", data.Until)
		if err != nil {
			http.Error(w, "Invalid date format. Please use YYYY-MM-DD.", http.StatusBadRequest)
			return
		}
		untilDate = untilDate.AddDate(0, 0, 1)
	}

	// Scan directory for files
	type fileWithPath struct {
		info os.FileInfo
		path string
	}
	var allFiles []fileWithPath
	filepath.Walk(data.SourceDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && !strings.HasPrefix(info.Name(), "._") {
			allFiles = append(allFiles, fileWithPath{info: info, path: path})
		}
		return nil
	})

	// Build imported files set if skip duplicates enabled
	var importedFiles map[string]bool
	if data.SkipDuplicates {
		importedFiles = buildImportedFilesSet()
	}

	destinationDirCreated := !isNewBatch
	copiedCount := 0
	skippedDuplicates := 0
	var copiedFiles []string

	for _, fileEntry := range allFiles {
		file := fileEntry.info
		lowerName := strings.ToLower(file.Name())
		isImage := strings.HasSuffix(lowerName, ".jpg") || strings.HasSuffix(lowerName, ".jpeg") ||
			strings.HasSuffix(lowerName, ".png") || strings.HasSuffix(lowerName, ".gif")
		isMp4 := strings.HasSuffix(lowerName, ".mp4")
		isRaw := strings.HasSuffix(lowerName, ".cr3") || strings.HasSuffix(lowerName, ".cr2") ||
			strings.HasSuffix(lowerName, ".arw") || strings.HasSuffix(lowerName, ".nef") ||
			strings.HasSuffix(lowerName, ".raf") || strings.HasSuffix(lowerName, ".dng")

		if !isImage && !isRaw && (!isMp4 || !data.ImportVideos) {
			continue
		}

		// Check date filter
		modTime := file.ModTime()
		if !sinceDate.IsZero() && modTime.Before(sinceDate) {
			continue
		}
		if !untilDate.IsZero() && !modTime.Before(untilDate) {
			continue
		}

		// Check duplicates
		if data.SkipDuplicates && importedFiles[file.Name()] {
			skippedDuplicates++
			continue
		}

		destinationFile := filepath.Join(destinationDir, file.Name())
		if _, err := os.Stat(destinationFile); err == nil {
			continue
		}

		// Create destination directory on first file
		if !destinationDirCreated {
			if err := os.MkdirAll(destinationDir, 0755); err != nil {
				http.Error(w, "Could not create destination directory", http.StatusInternalServerError)
				return
			}
			destinationDirCreated = true
		}

		// Copy file
		source, err := os.Open(fileEntry.path)
		if err != nil {
			log.Printf("Failed to open source file: %v", err)
			continue
		}
		defer source.Close()

		destination, err := os.Create(destinationFile)
		if err != nil {
			log.Printf("Failed to create destination file: %v", err)
			continue
		}
		defer destination.Close()

		if _, err := io.Copy(destination, source); err != nil {
			log.Printf("Failed to copy file: %v", err)
			continue
		}

		copiedCount++
		// Add both images and raw files for thumbnail generation
		if isImage || isRaw {
			copiedFiles = append(copiedFiles, file.Name())
		}
	}

	// Handle case where no files were copied
	if copiedCount == 0 {
		var message string
		if !sinceDate.IsZero() || !untilDate.IsZero() {
			message = "No new files found in the selected date range"
		} else if skippedDuplicates > 0 {
			message = "All " + strconv.Itoa(skippedDuplicates) + " files have already been imported."
		} else {
			message = "No files found to import."
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message":       message,
			"new_directory": nil,
		})
		return
	}

	// Save recent paths
	saveRecentPath(data.SourceDirectory, "source")
	if data.DestinationBase != "" {
		saveRecentPath(data.DestinationBase, "destination")
	}

	// Start async thumbnail generation; use full destinationDir so resolvePhotoDir works correctly
	dirName := filepath.Base(destinationDir)
	go func() {
		log.Printf("Starting background thumbnail generation for imported directory: %s (%d photos)", destinationDir, len(copiedFiles))
		preGenerateThumbnails(destinationDir, copiedFiles)
	}()

	message := "Successfully copied " + strconv.Itoa(copiedCount) + " new files"
	if !isNewBatch {
		message += " to " + dirName
	}
	message += "."
	if skippedDuplicates > 0 {
		message += " Skipped " + strconv.Itoa(skippedDuplicates) + " already imported."
	}

	w.Header().Set("Content-Type", "application/json")
	var newDirectory interface{}
	if isNewBatch {
		// Return full path so frontend can resolve photos from the correct location
		newDirectory = destinationDir
	} else {
		newDirectory = nil
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":       message,
		"new_directory": newDirectory,
	})
}

func extractRawPreview(directory, filename string) (string, error) {
	// Check cache first
	previewDir := filepath.Join(rawPreviewCacheDir, directoryCacheKey(directory))
	previewPath := filepath.Join(previewDir, filename+".jpg")

	// Return cached preview if it exists
	if _, err := os.Stat(previewPath); err == nil {
		return previewPath, nil
	}

	// Create preview cache directory
	if err := os.MkdirAll(previewDir, 0755); err != nil {
		return "", err
	}

	rawFilePath := filepath.Join(resolvePhotoDir(directory), filename)

	// Try multiple extraction methods in order of preference
	extractionMethods := [][]string{
		{"-b", "-PreviewImage", rawFilePath},
		{"-b", "-JpgFromRaw", rawFilePath},
		{"-b", "-OtherImage", rawFilePath},
		{"-b", "-ThumbnailImage", rawFilePath},
	}

	var output []byte
	var err error
	var lastError error

	for _, args := range extractionMethods {
		cmd := exec.Command("exiftool", args...)
		output, err = cmd.CombinedOutput()
		if err == nil && len(output) > 0 {
			// Check if output looks like JPEG (starts with FFD8)
			if len(output) > 2 && output[0] == 0xFF && output[1] == 0xD8 {
				break
			}
		}
		lastError = err
	}

	if err != nil || len(output) == 0 {
		if lastError != nil {
			log.Printf("Failed to extract preview from %s: %v (output: %s)", filename, lastError, string(output))
		} else {
			log.Printf("Failed to extract preview from %s: no valid JPEG data found", filename)
		}
		return "", lastError
	}

	// Write preview to cache
	if err := ioutil.WriteFile(previewPath, output, 0644); err != nil {
		return "", err
	}

	log.Printf("Extracted preview for raw file: %s (%d bytes)", filename, len(output))
	return previewPath, nil
}

func generateThumbnail(directory, filename string) error {
	thumbnailDir := filepath.Join(thumbnailCacheDir, directoryCacheKey(directory))
	thumbnailPath := filepath.Join(thumbnailDir, filename)

	// Check if thumbnail already exists
	if _, err := os.Stat(thumbnailPath); err == nil {
		return nil // Already exists
	}

	originalPhotoPath := filepath.Join(resolvePhotoDir(directory), filename)

	var img image.Image
	if isRawFile(filename) {
		jpegData, err := extractEmbeddedJPEG(originalPhotoPath)
		if err != nil {
			return fmt.Errorf("extracting embedded JPEG from %s: %w", filename, err)
		}
		img, err = jpeg.Decode(bytes.NewReader(jpegData))
		if err != nil {
			return fmt.Errorf("decoding embedded JPEG from %s: %w", filename, err)
		}
	} else {
		file, err := os.Open(originalPhotoPath)
		if err != nil {
			return err
		}
		defer file.Close()
		img, _, err = image.Decode(file)
		if err != nil {
			return err
		}
	}

	thumb := resize.Thumbnail(uint(thumbnailSize), uint(thumbnailSize), img, resize.Lanczos3)

	if err := os.MkdirAll(thumbnailDir, 0755); err != nil {
		return err
	}

	out, err := os.Create(thumbnailPath)
	if err != nil {
		return err
	}
	defer out.Close()

	return jpeg.Encode(out, thumb, nil)
}

func preGenerateThumbnails(directory string, photos []string) {
	const numWorkers = 20
	var wg sync.WaitGroup
	photoChan := make(chan string, len(photos))

	// Start worker goroutines
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for filename := range photoChan {
				if err := generateThumbnail(directory, filename); err != nil {
					log.Printf("Failed to generate thumbnail for %s: %v", filename, err)
				}
			}
		}()
	}

	// Send photos to workers
	for _, photo := range photos {
		photoChan <- photo
	}
	close(photoChan)

	// Wait for all workers to complete
	wg.Wait()
	log.Printf("Completed thumbnail generation for directory: %s (%d photos)", directory, len(photos))
}

func servePhotoHandler(w http.ResponseWriter, r *http.Request) {
	directory := r.URL.Query().Get("dir")
	filename := strings.TrimPrefix(r.URL.Path, "/photos/")

	if directory == "" || filename == "" {
		http.Error(w, "Invalid photo path", http.StatusBadRequest)
		return
	}

	// Check if it's a raw file
	if isRawFile(filename) {
		// Extract and serve the preview
		previewPath, err := extractRawPreview(directory, filename)
		if err != nil {
			http.Error(w, "Failed to extract raw preview", http.StatusInternalServerError)
			log.Printf("Error extracting raw preview for %s: %v", filename, err)
			return
		}
		http.ServeFile(w, r, previewPath)
		return
	}

	// Serve regular image file
	photoPath := filepath.Join(resolvePhotoDir(directory), filename)
	http.ServeFile(w, r, photoPath)
}

func serveThumbnailHandler(w http.ResponseWriter, r *http.Request) {
	directory := r.URL.Query().Get("dir")
	filename := strings.TrimPrefix(r.URL.Path, "/thumbnail/")

	if directory == "" || filename == "" {
		http.Error(w, "Invalid thumbnail path", http.StatusBadRequest)
		return
	}

	thumbnailDir := filepath.Join(thumbnailCacheDir, directoryCacheKey(directory))
	thumbnailPath := filepath.Join(thumbnailDir, filename)

	// Generate thumbnail on-demand if it doesn't exist
	if _, err := os.Stat(thumbnailPath); err != nil {
		if err := generateThumbnail(directory, filename); err != nil {
			http.Error(w, "Failed to generate thumbnail", http.StatusInternalServerError)
			log.Printf("Error generating thumbnail for %s/%s: %v", directory, filename, err)
			return
		}
	}

	// RAW thumbnails are JPEG bytes stored under the raw filename — set content type explicitly.
	if isRawFile(filename) {
		f, err := os.Open(thumbnailPath)
		if err != nil {
			http.Error(w, "Failed to serve thumbnail", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		stat, err := f.Stat()
		if err != nil {
			http.Error(w, "Failed to stat thumbnail", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		http.ServeContent(w, r, filename+".jpg", stat.ModTime(), f)
		return
	}

	http.ServeFile(w, r, thumbnailPath)
}
