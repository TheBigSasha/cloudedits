// Package formats defines the supported photo/RAW media formats, MIME types,
// and filesystem exclusion rules for cloudedits.
package formats

import (
	"bytes"
	"path/filepath"
	"strings"
)

var rasterMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".avif": "image/avif",
	".jxl":  "image/jxl",
	".heic": "image/heic",
	".heif": "image/heif",
	".hif":  "image/heif",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".exr":  "image/x-exr",
}

// rawMIME lists all 20 supported camera RAW extensions.
var rawMIME = map[string]string{
	".cr2": "image/x-canon-cr2",
	".cr3": "image/x-canon-cr3",
	".nef": "image/x-nikon-nef",
	".nrw": "image/x-nikon-nrw",
	".arw": "image/x-sony-arw",
	".dng": "image/x-adobe-dng",
	".orf": "image/x-olympus-orf",
	".rw2": "image/x-panasonic-rw2",
	".pef": "image/x-pentax-pef",
	".raf": "image/x-fuji-raf",
	".crw": "image/x-canon-crw",
	".rwl": "image/x-leica-rwl",
	".srw": "image/x-samsung-srw",
	".kdc": "image/x-kodak-kdc",
	".dcr": "image/x-kodak-dcr",
	".mrw": "image/x-minolta-mrw",
	".3fr": "image/x-hasselblad-3fr",
	".fff": "image/x-hasselblad-fff",
	".iiq": "image/x-phaseone-iiq",
	".x3f": "image/x-sigma-x3f",
}

// ShouldSkipName returns true for directories or files that must never be
// scanned as photo assets (AppleDouble ._*, .hdrtoys, .DS_Store, dotfiles).
func ShouldSkipName(name string) bool {
	if name == "" {
		return true
	}
	base := filepath.Base(name)
	if base == ".hdrtoys" || base == ".DS_Store" || base == "Thumbs.db" {
		return true
	}
	if strings.HasPrefix(base, "._") {
		return true
	}
	if strings.HasPrefix(base, ".") {
		return true
	}
	return false
}

// EditThumbnailSuffix names the edited-preview artifact of a photo:
// "<photo filename>.edit_thumbnail.jpg".
const EditThumbnailSuffix = ".edit_thumbnail.jpg"

// IsEditThumbnailName reports whether name is an edited-preview artifact. An
// editor that keeps its artifacts beside the photos writes these into the
// photo folder itself, where they carry a .jpg extension but are a derived
// preview of another asset, never a photo. The suffix match is
// case-insensitive and requires a source filename in front of it.
func IsEditThumbnailName(name string) bool {
	base := filepath.Base(name)
	return len(base) > len(EditThumbnailSuffix) && strings.EqualFold(base[len(base)-len(EditThumbnailSuffix):], EditThumbnailSuffix)
}

// auxChannelSuffixes maps recognized auxiliary channel names to their canonical
// compound filename suffixes appended to a photo filename
// (e.g. "<photo filename>.depth.png").
var auxChannelSuffixes = map[string]string{
	"depth": ".depth.png",
}

// AuxChannelSuffix returns the canonical filename suffix (such as ".depth.png")
// for a recognized auxiliary channel name.
func AuxChannelSuffix(channel string) (string, bool) {
	suffix, ok := auxChannelSuffixes[channel]
	return suffix, ok
}

// IsAuxChannelName reports whether name is a recognized auxiliary channel
// sidecar artifact (for example "<photo filename>.depth.png"). An editor that
// keeps artifacts beside the photos writes these into the photo folder itself,
// where they carry a .png extension but are derived channels of another asset,
// never standalone photos. The suffix match is case-insensitive and requires a
// source filename in front of it.
func IsAuxChannelName(name string) bool {
	base := filepath.Base(name)
	for _, suffix := range auxChannelSuffixes {
		if len(base) > len(suffix) && strings.EqualFold(base[len(base)-len(suffix):], suffix) {
			return true
		}
	}
	return false
}

// IsSupportedFile returns true if filename has a supported image or RAW
// extension and is not an excluded AppleDouble/hidden file, an edited-preview
// artifact, or an auxiliary channel sidecar.
func IsSupportedFile(name string) bool {
	if ShouldSkipName(name) || IsEditThumbnailName(name) || IsAuxChannelName(name) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	if _, ok := rasterMIME[ext]; ok {
		return true
	}
	_, ok := rawMIME[ext]
	return ok
}

// IsRAW returns true if filename has a supported camera RAW extension.
func IsRAW(name string) bool {
	if ShouldSkipName(name) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	_, ok := rawMIME[ext]
	return ok
}

// Kind returns the format family for filename ("raw", "exr", "jpeg", "png", or
// "image" for other raster formats). Call AssetKind when header probing has
// also determined whether the asset carries an UltraHDR gain map.
func Kind(name string) string {
	if IsRAW(name) {
		return "raw"
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".exr":
		return "exr"
	case ".jpg", ".jpeg":
		return "jpeg"
	case ".png":
		return "png"
	default:
		return "image"
	}
}

// AssetKind returns the specific LP/2 asset kind for filename, classifying
// non-RAW assets carrying an UltraHDR gain map as "ultrahdr".
func AssetKind(name string, hasGainMap bool) string {
	if IsRAW(name) {
		return "raw"
	}
	if hasGainMap {
		return "ultrahdr"
	}
	return Kind(name)
}

// MIME returns the canonical MIME type for a supported file, or
// "application/octet-stream" if unknown.
func MIME(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if m, ok := rasterMIME[ext]; ok {
		return m
	}
	if m, ok := rawMIME[ext]; ok {
		return m
	}
	return "application/octet-stream"
}

// IsJPEGMagic reports whether data starts with the JPEG SOI marker (FF D8 FF).
func IsJPEGMagic(data []byte) bool {
	return len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF
}

var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

// IsPNGMagic reports whether data starts with the 8-byte PNG signature
// (\x89PNG\r\n\x1a\n).
func IsPNGMagic(data []byte) bool {
	return bytes.HasPrefix(data, pngMagic)
}

// SupportedExtensions returns a copy of all supported extensions (lowercased).
func SupportedExtensions() []string {
	out := make([]string, 0, len(rasterMIME)+len(rawMIME))
	for ext := range rasterMIME {
		out = append(out, ext)
	}
	for ext := range rawMIME {
		out = append(out, ext)
	}
	return out
}
