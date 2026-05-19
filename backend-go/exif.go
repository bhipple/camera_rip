package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type photoExifData struct {
	Make        string
	Model       string
	DateTaken   string
	ShutterNum  uint32
	ShutterDen  uint32
	FNumber     float64
	ISO         uint32
	FocalLength float64
	Width       uint32
	Height      uint32
}

// extractJPEGExifBlock returns the TIFF bytes from a JPEG APP1 "Exif\0\0" segment.
func extractJPEGExifBlock(data []byte) []byte {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil
	}
	pos := 2
	for pos+4 <= len(data) {
		if data[pos] != 0xFF {
			break
		}
		marker := data[pos+1]
		segLen := int(data[pos+2])<<8 | int(data[pos+3])
		if segLen < 2 {
			break
		}
		segEnd := pos + 2 + segLen
		if marker == 0xE1 && pos+10 <= len(data) && string(data[pos+4:pos+10]) == "Exif\x00\x00" {
			end := segEnd
			if end > len(data) {
				end = len(data)
			}
			if pos+10 < end {
				return data[pos+10 : end]
			}
		}
		if segEnd > len(data) {
			break
		}
		pos = segEnd
	}
	return nil
}

func exifReadASCII(data []byte, bo binary.ByteOrder, count uint32, vf []byte) string {
	if count == 0 {
		return ""
	}
	var raw []byte
	if count <= 4 {
		if uint32(len(vf)) < count {
			return ""
		}
		raw = vf[:count]
	} else {
		off := bo.Uint32(vf)
		if uint64(off)+uint64(count) > uint64(len(data)) {
			return ""
		}
		raw = data[off : off+count]
	}
	return strings.TrimRight(string(raw), "\x00 ")
}

func exifReadRational(data []byte, bo binary.ByteOrder, vf []byte) (uint32, uint32) {
	off := bo.Uint32(vf)
	if int(off)+8 > len(data) {
		return 0, 0
	}
	return bo.Uint32(data[off:]), bo.Uint32(data[off+4:])
}

func exifReadUint(data []byte, bo binary.ByteOrder, typ uint16, count uint32, vf []byte) uint32 {
	switch typ {
	case 3: // SHORT
		if count == 1 {
			return uint32(bo.Uint16(vf))
		}
		off := bo.Uint32(vf)
		if int(off)+2 <= len(data) {
			return uint32(bo.Uint16(data[off:]))
		}
	case 4: // LONG
		if count == 1 {
			return bo.Uint32(vf)
		}
		off := bo.Uint32(vf)
		if int(off)+4 <= len(data) {
			return bo.Uint32(data[off:])
		}
	}
	return 0
}

func parseTIFFExifIFD(data []byte, bo binary.ByteOrder, ifdOff uint32, out *photoExifData, depth int) {
	if depth > 3 || int(ifdOff)+2 > len(data) {
		return
	}
	n := int(bo.Uint16(data[ifdOff:]))
	base := int(ifdOff) + 2
	for i := 0; i < n; i++ {
		e := base + i*12
		if e+12 > len(data) {
			break
		}
		tag := bo.Uint16(data[e:])
		typ := bo.Uint16(data[e+2:])
		count := bo.Uint32(data[e+4:])
		vf := data[e+8 : e+12]

		switch tag {
		case 0x010F:
			out.Make = exifReadASCII(data, bo, count, vf)
		case 0x0110:
			out.Model = exifReadASCII(data, bo, count, vf)
		case 0x0100:
			if out.Width == 0 {
				out.Width = exifReadUint(data, bo, typ, count, vf)
			}
		case 0x0101:
			if out.Height == 0 {
				out.Height = exifReadUint(data, bo, typ, count, vf)
			}
		case 0x0132:
			if out.DateTaken == "" {
				out.DateTaken = exifReadASCII(data, bo, count, vf)
			}
		case 0x9003:
			out.DateTaken = exifReadASCII(data, bo, count, vf)
		case 0x829A:
			out.ShutterNum, out.ShutterDen = exifReadRational(data, bo, vf)
		case 0x829D:
			fn, fd := exifReadRational(data, bo, vf)
			if fd != 0 {
				out.FNumber = float64(fn) / float64(fd)
			}
		case 0x8827:
			out.ISO = exifReadUint(data, bo, typ, count, vf)
		case 0x920A:
			fln, fld := exifReadRational(data, bo, vf)
			if fld != 0 {
				out.FocalLength = float64(fln) / float64(fld)
			}
		case 0xA002:
			out.Width = exifReadUint(data, bo, typ, count, vf)
		case 0xA003:
			out.Height = exifReadUint(data, bo, typ, count, vf)
		case 0x8769:
			parseTIFFExifIFD(data, bo, bo.Uint32(vf), out, depth+1)
		}
	}
}

// parseTIFFData parses a raw TIFF blob (must start with II/MM magic) into out.
func parseTIFFData(tiff []byte, out *photoExifData) {
	if len(tiff) < 8 {
		return
	}
	if (tiff[0] != 'I' || tiff[1] != 'I') && (tiff[0] != 'M' || tiff[1] != 'M') {
		return
	}
	var bo binary.ByteOrder
	if tiff[0] == 'I' {
		bo = binary.LittleEndian
	} else {
		bo = binary.BigEndian
	}
	if bo.Uint16(tiff[2:]) != 42 {
		return
	}
	parseTIFFExifIFD(tiff, bo, bo.Uint32(tiff[4:]), out, 0)
}

func parsePhotoExif(filePath string) *photoExifData {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return &photoExifData{}
	}
	out := &photoExifData{}

	lower := strings.ToLower(filePath)
	switch {
	case strings.HasSuffix(lower, ".orf"):
		parseTIFFData(data, out)
	case strings.HasSuffix(lower, ".cr3"):
		// CR3 (ISOBMFF/CRX) stores EXIF in CMT1 (IFD0: Make/Model/DateTime) and
		// CMT2 (ExifIFD: exposure settings) boxes, not in any embedded JPEG APP1.
		for _, boxType := range [][]byte{[]byte("CMT1"), []byte("CMT2")} {
			if idx := bytes.Index(data, boxType); idx >= 4 {
				parseTIFFData(data[idx+4:], out)
			}
		}
	default:
		parseTIFFData(extractJPEGExifBlock(data), out)
	}

	return out
}

func gcdUint32(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func photoInfoHandler(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	filename := r.URL.Query().Get("filename")
	if dir == "" || filename == "" {
		http.Error(w, "Missing dir or filename", http.StatusBadRequest)
		return
	}

	dir = resolvePhotoDir(dir)
	filePath := filepath.Join(dir, filename)
	if !strings.HasPrefix(filepath.Clean(filePath), filepath.Clean(dir)+string(os.PathSeparator)) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	stat, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Stat error", http.StatusInternalServerError)
		return
	}

	exif := parsePhotoExif(filePath)

	if (exif.Width == 0 || exif.Height == 0) && !isRawFile(filename) {
		if f, ferr := os.Open(filePath); ferr == nil {
			if cfg, derr := jpeg.DecodeConfig(f); derr == nil {
				exif.Width = uint32(cfg.Width)
				exif.Height = uint32(cfg.Height)
			}
			f.Close()
		}
	}

	type infoResp struct {
		Filename     string `json:"filename"`
		DateTaken    string `json:"date_taken,omitempty"`
		CameraMake   string `json:"camera_make,omitempty"`
		CameraModel  string `json:"camera_model,omitempty"`
		FNumber      string `json:"f_number,omitempty"`
		ShutterSpeed string `json:"shutter_speed,omitempty"`
		ISO          string `json:"iso,omitempty"`
		FocalLength  string `json:"focal_length,omitempty"`
		Width        uint32 `json:"width"`
		Height       uint32 `json:"height"`
		Megapixels   string `json:"megapixels,omitempty"`
		FileSize     int64  `json:"file_size"`
	}

	resp := infoResp{
		Filename:    filename,
		FileSize:    stat.Size(),
		Width:       exif.Width,
		Height:      exif.Height,
		DateTaken:   exif.DateTaken,
		CameraMake:  exif.Make,
		CameraModel: exif.Model,
	}

	if exif.FNumber > 0 {
		if exif.FNumber == float64(int(exif.FNumber)) {
			resp.FNumber = fmt.Sprintf("f/%.0f", exif.FNumber)
		} else {
			resp.FNumber = fmt.Sprintf("f/%.1f", exif.FNumber)
		}
	}
	if exif.ShutterDen > 0 {
		if exif.ShutterNum >= exif.ShutterDen {
			secs := float64(exif.ShutterNum) / float64(exif.ShutterDen)
			if secs == float64(int(secs)) {
				resp.ShutterSpeed = fmt.Sprintf("%.0fs", secs)
			} else {
				resp.ShutterSpeed = fmt.Sprintf("%.1fs", secs)
			}
		} else {
			g := gcdUint32(exif.ShutterNum, exif.ShutterDen)
			if g == 0 {
				g = 1
			}
			resp.ShutterSpeed = fmt.Sprintf("%d/%d", exif.ShutterNum/g, exif.ShutterDen/g)
		}
	}
	if exif.ISO > 0 {
		resp.ISO = fmt.Sprintf("%d", exif.ISO)
	}
	if exif.FocalLength > 0 {
		if exif.FocalLength == float64(int(exif.FocalLength)) {
			resp.FocalLength = fmt.Sprintf("%.0fmm", exif.FocalLength)
		} else {
			resp.FocalLength = fmt.Sprintf("%.1fmm", exif.FocalLength)
		}
	}
	if exif.Width > 0 && exif.Height > 0 {
		mp := float64(exif.Width) * float64(exif.Height) / 1e6
		resp.Megapixels = fmt.Sprintf("%.1fMP", mp)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
