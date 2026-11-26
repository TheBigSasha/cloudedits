package formats

import "testing"

func TestSupportedFormatsAndExclusions(t *testing.T) {
	supported := []string{
		"photo.jpg", "PHOTO.JPEG", "a.png", "b.webp", "c.avif", "d.jxl",
		"e.heic", "f.heif", "g.hif", "h.tif", "i.tiff", "j.exr",
		// All 20 camera RAW extensions
		"r1.cr2", "r2.cr3", "r3.nef", "r4.nrw", "r5.arw", "r6.dng", "r7.orf",
		"r8.rw2", "r9.pef", "r10.raf", "r11.crw", "r12.rwl", "r13.srw", "r14.kdc",
		"r15.dcr", "r16.mrw", "r17.3fr", "r18.fff", "r19.iiq", "r20.x3f",
	}
	for _, f := range supported {
		if !IsSupportedFile(f) {
			t.Errorf("expected %q to be supported", f)
		}
		if MIME(f) == "application/octet-stream" {
			t.Errorf("expected known MIME for %q", f)
		}
	}

	rawCount := 0
	for _, f := range supported {
		if IsRAW(f) {
			rawCount++
			if Kind(f) != "raw" {
				t.Errorf("expected Kind(%q)=raw, got %q", f, Kind(f))
			}
		} else if Kind(f) != "image" {
			t.Errorf("expected Kind(%q)=image, got %q", f, Kind(f))
		}
	}
	if rawCount != 20 {
		t.Fatalf("expected 20 RAW formats, got %d", rawCount)
	}

	excluded := []string{
		"._IMG_0001.JPG",
		"._DSC0001.ARW",
		".DS_Store",
		".hdrtoys",
		".hidden.jpg",
		"notes.txt",
		"IMG_0001.ARW.edits",
		"movie.mp4",
	}
	for _, f := range excluded {
		if IsSupportedFile(f) {
			t.Errorf("expected %q to be excluded", f)
		}
	}
}

func TestJPEGMagic(t *testing.T) {
	if !IsJPEGMagic([]byte{0xFF, 0xD8, 0xFF, 0xE0}) {
		t.Fatal("expected valid JPEG magic")
	}
	if IsJPEGMagic([]byte{0x89, 0x50, 0x4E, 0x47}) {
		t.Fatal("PNG must not match JPEG magic")
	}
	if IsJPEGMagic([]byte{0xFF, 0xD8}) {
		t.Fatal("truncated bytes must not match JPEG magic")
	}
}

func TestPNGMagic(t *testing.T) {
	if !IsPNGMagic([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n', 0x00}) {
		t.Fatal("expected valid PNG magic")
	}
	if IsPNGMagic([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F'}) {
		t.Fatal("JPEG must not match PNG magic")
	}
	if IsPNGMagic([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A}) {
		t.Fatal("truncated PNG signature must not match")
	}
	if IsPNGMagic(nil) {
		t.Fatal("empty slice must not match PNG magic")
	}
}

// An editor that keeps its artifacts beside the photos writes the edited
// preview as "<photo>.edit_thumbnail.jpg". It is a derived preview of another
// asset, never a photo of its own.
func TestEditThumbnailBesidePhotoIsNotAnAsset(t *testing.T) {
	for _, name := range []string{"IMG_0001.JPG.edit_thumbnail.jpg", "IMG_0001.ARW.EDIT_THUMBNAIL.JPG", "DCIM/a.jpg.edit_thumbnail.jpg"} {
		if IsSupportedFile(name) {
			t.Errorf("%q is an edited-preview artifact but was accepted as a photo", name)
		}
	}
	// With nothing before the suffix there is no source photo, so it is an
	// ordinary file.
	for _, name := range []string{".edit_thumbnail.jpg.png", "edit_thumbnail.jpg", "IMG_0001.JPG"} {
		if got, want := IsSupportedFile(name), name != ".edit_thumbnail.jpg.png"; got != want {
			t.Errorf("IsSupportedFile(%q) = %v, want %v", name, got, want)
		}
	}
}

// Auxiliary channel sidecars (such as "<photo>.depth.png") may sit beside
// photos in read-only colocated layouts; they carry a .png extension but are
// derived channels of another asset, never standalone photos.
func TestAuxChannelBesidePhotoIsNotAnAsset(t *testing.T) {
	if suffix, ok := AuxChannelSuffix("depth"); !ok || suffix != ".depth.png" {
		t.Fatalf("AuxChannelSuffix(depth) = (%q, %v), want (.depth.png, true)", suffix, ok)
	}
	if suffix, ok := AuxChannelSuffix("unknown"); ok || suffix != "" {
		t.Fatalf("AuxChannelSuffix(unknown) = (%q, %v), want (\"\", false)", suffix, ok)
	}

	for _, name := range []string{
		"IMG_0001.JPG.depth.png",
		"IMG_0001.ARW.DEPTH.PNG",
		"DCIM/a.jpg.depth.png",
		"scene.depth.png",
	} {
		if !IsAuxChannelName(name) {
			t.Errorf("IsAuxChannelName(%q) = false, want true", name)
		}
		if IsSupportedFile(name) {
			t.Errorf("%q is an auxiliary channel sidecar but was accepted as a photo", name)
		}
	}

	for _, name := range []string{"depth.png", "photo.png", "my.photo.png", "IMG_0001.JPG"} {
		if IsAuxChannelName(name) {
			t.Errorf("IsAuxChannelName(%q) = true, want false", name)
		}
		if !IsSupportedFile(name) {
			t.Errorf("IsSupportedFile(%q) = false, want true", name)
		}
	}
	if IsSupportedFile(".depth.png") {
		t.Errorf("IsSupportedFile(.depth.png) = true, want false")
	}
}
