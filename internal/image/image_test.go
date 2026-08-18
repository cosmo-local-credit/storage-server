package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"testing"

	"github.com/disintegration/imaging"
	"github.com/gen2brain/webp"
	xwebp "golang.org/x/image/webp"
)

const testMaxPixels = 12_500_000

// normalize runs the two-step pipeline the handler uses.
func normalize(t *testing.T, src []byte, width int) Result {
	t.Helper()
	return normalizeWith(t, src, width, DefaultOpts())
}

func normalizeWith(t *testing.T, src []byte, width int, opts Opts) Result {
	t.Helper()
	info, err := Inspect(src, testMaxPixels)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	got, err := Normalize(src, info, width, opts)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return got
}

func TestInspectRejectsUnsupported(t *testing.T) {
	if _, err := Inspect([]byte("%PDF-1.1 not an image"), testMaxPixels); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestInspectRejectsOversizedPixels(t *testing.T) {
	if _, err := Inspect(photoJPEG(t, 16, 16), 10); !errors.Is(err, ErrTooManyPixels) {
		t.Fatalf("err = %v, want ErrTooManyPixels", err)
	}
}

func TestInspectReportsWireDimensions(t *testing.T) {
	info, err := Inspect(photoJPEG(t, 320, 200), testMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "jpg" || info.Width != 320 || info.Height != 200 {
		t.Fatalf("info = %+v, want jpg 320x200", info)
	}
}

func TestNormalizeRejectsInvalidWidth(t *testing.T) {
	src := photoJPEG(t, 32, 24)
	info, err := Inspect(src, testMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Normalize(src, info, 0, DefaultOpts()); !errors.Is(err, ErrInvalidWidth) {
		t.Fatalf("err = %v, want ErrInvalidWidth", err)
	}
}

func TestNormalizeRejectsZeroInfo(t *testing.T) {
	if _, err := Normalize(photoJPEG(t, 32, 24), Info{}, 400, DefaultOpts()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestNormalizeNeverUpscales(t *testing.T) {
	got := normalize(t, photoJPEG(t, 320, 200), 800)
	if got.Width != 320 || got.Height != 200 {
		t.Fatalf("size = %dx%d, want 320x200", got.Width, got.Height)
	}
}

func TestNormalizeOutputNeverExceedsBounds(t *testing.T) {
	src := photoJPEG(t, 640, 400)
	for _, width := range []int{400, 800, 1280} {
		got := normalize(t, src, width)
		if got.Width > 640 || got.Width > width {
			t.Fatalf("width %d exceeds min(source=640, requested=%d)", got.Width, width)
		}
	}
}

func TestNormalizeResizedJPEGIsLossyWebP(t *testing.T) {
	src := photoJPEG(t, 1600, 1000)
	got := normalize(t, src, 400)
	if got.Width != 400 || got.Height != 250 {
		t.Fatalf("size = %dx%d, want 400x250", got.Width, got.Height)
	}
	if got.Extension != "webp" || got.ContentType != "image/webp" {
		t.Fatalf("format = %s %s", got.Extension, got.ContentType)
	}
	if got.Lossless {
		t.Fatal("expected lossy webp for a resized photographic jpeg")
	}
	assertMinPSNR(t, src, got, DefaultOpts().MinPSNR)
	assertSizeCeiling(t, got, 9_000)
}

func TestNormalizeResizedPNGPhotoIsLossy(t *testing.T) {
	// A photograph in a PNG container must not be forced through lossless
	// encoding: the container says nothing about the content.
	src := encodePNG(t, photoNRGBA(1600, 1000))
	got := normalize(t, src, 400)
	if got.Lossless {
		t.Fatal("png photo was encoded lossless")
	}
	if got.Extension != "webp" {
		t.Fatalf("ext = %s, want webp", got.Extension)
	}
	assertMinPSNR(t, src, got, DefaultOpts().MinPSNR)
	assertSizeCeiling(t, got, 9_000)
}

func TestNormalizeLogoAndTextAreLossless(t *testing.T) {
	// Both containers must reach the same decision for the same picture.
	for _, tc := range []struct {
		name string
		src  []byte
	}{
		{"png", logoPNG(t, 1200, 800)},
		{"jpeg", logoJPEG(t, 1200, 800)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalize(t, tc.src, 400)
			if !got.Lossless || got.Extension != "webp" {
				t.Fatalf("lossless=%v ext=%s, want lossless webp", got.Lossless, got.Extension)
			}
			if got.Width != 400 {
				t.Fatalf("width = %d, want 400", got.Width)
			}
			assertSizeCeiling(t, got, 400)
		})
	}
}

func TestNormalizeGradientAndFaceStayLossy(t *testing.T) {
	// Smooth colour ramps and skin tones score badly on PSNR because chroma
	// subsampling hits them hardest, but lossless is far larger for both. They
	// must not be mistaken for graphics.
	for _, tc := range []struct {
		name    string
		src     []byte
		ceiling int
	}{
		{"gradient", gradientJPEG(t, 1600, 1200), 6_200},
		{"face", faceJPEG(t, 1280, 1280), 2_200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalize(t, tc.src, 800)
			if got.Lossless {
				t.Fatalf("%s was encoded lossless", tc.name)
			}
			assertSizeCeiling(t, got, tc.ceiling)
		})
	}
}

func TestNormalizePreservesAlpha(t *testing.T) {
	got := normalize(t, alphaPNG(t, 80, 60), 40)
	if !got.Lossless {
		t.Fatal("alpha input must stay lossless")
	}
	decoded, err := xwebp.Decode(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if !imageHasAlpha(decoded) {
		t.Fatal("alpha channel was dropped")
	}
}

func TestNormalizeKeepsAlreadyWebP(t *testing.T) {
	var buf bytes.Buffer
	if err := webp.Encode(&buf, photoNRGBA(240, 160), webp.Options{Quality: 82, Method: 6}); err != nil {
		t.Fatal(err)
	}
	src := buf.Bytes()
	got := normalize(t, src, 240)
	if !bytes.Equal(got.Bytes, src) {
		t.Fatalf("re-encoded an already-normalised webp (%d vs %d bytes)", len(got.Bytes), len(src))
	}
	if got.Extension != "webp" {
		t.Fatalf("ext = %s", got.Extension)
	}
}

func TestNormalizeSameSizeJPEGKeepsOriginalBytes(t *testing.T) {
	// The material-saving guard is driven from the configured ratio rather than
	// from a fixture that happens to resist WebP, so both of its branches are
	// reachable. A ratio this strict can never be met, so the source must come
	// back byte for byte with its own content type and extension.
	opts := DefaultOpts()
	opts.MateriallySmallerRatio = 0.001

	src := photoJPEG(t, 64, 48)
	got := normalizeWith(t, src, 64, opts)
	if !bytes.Equal(got.Bytes, src) {
		t.Fatalf("source was re-encoded: %d bytes in, %d out", len(src), len(got.Bytes))
	}
	if got.Extension != "jpg" || got.ContentType != "image/jpeg" {
		t.Fatalf("ext=%s ctype=%s, want jpg/image/jpeg", got.Extension, got.ContentType)
	}
}

func TestNormalizeSameSizeConvertsWhenMateriallySmaller(t *testing.T) {
	// The counterpart to the test above: a same-size source that WebP does beat
	// by more than the ratio must be converted.
	src := photoJPEG(t, 600, 400)
	got := normalize(t, src, 600)
	if got.Extension != "webp" {
		t.Fatalf("ext = %s, want webp", got.Extension)
	}
	if !materiallySmaller(got.Bytes, src, DefaultOpts().MateriallySmallerRatio) {
		t.Fatalf("converted without a material saving: %d vs %d", len(got.Bytes), len(src))
	}
}

func TestNormalizeEXIFOrientations(t *testing.T) {
	raw := encodeJPEG(t, orientationProbe(64, 32), 95)
	for o := 1; o <= 8; o++ {
		src := jpegWithOrientation(raw, uint16(o))
		got := normalize(t, src, 64)

		wantW, wantH := 64, 32
		if o >= 5 {
			wantW, wantH = 32, 64
		}
		if got.Width != wantW || got.Height != wantH {
			t.Fatalf("orientation %d reported %dx%d, want %dx%d", o, got.Width, got.Height, wantW, wantH)
		}
		// The reported size must describe the stored pixels, not just the Result
		// struct, and the orientation tag must not survive into the object.
		assertStoredDimensions(t, got)
		if bytes.Contains(got.Bytes, []byte("Exif")) {
			t.Fatalf("orientation %d: stored bytes still carry an EXIF block", o)
		}
	}
}

func TestNormalizeOrientation6RotatesPixels(t *testing.T) {
	// Orientation 6 means "rotate 90 degrees clockwise". The probe puts a red
	// marker in the top-left corner, which must land in the top-right corner of
	// the stored image.
	raw := encodeJPEG(t, orientationProbe(64, 32), 95)
	got := normalize(t, jpegWithOrientation(raw, 6), 64)
	if got.Width >= got.Height {
		t.Fatalf("orientation 6 should be portrait, got %dx%d", got.Width, got.Height)
	}
	assertStoredDimensions(t, got)

	stored, err := xwebp.Decode(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	b := stored.Bounds()
	if !isReddish(stored.At(b.Max.X-1, b.Min.Y)) {
		t.Fatal("top-right pixel is not the red marker: pixels were not rotated")
	}
	if isReddish(stored.At(b.Min.X, b.Min.Y)) {
		t.Fatal("red marker is still in the top-left corner: orientation was ignored")
	}
}

func TestNormalizeRotatedSourceIsNeverRetained(t *testing.T) {
	// A quarter-turn source must be re-encoded even when no resize is needed, or
	// the stored bytes would disagree with the width in the returned key.
	raw := encodeJPEG(t, smallFlat(64, 32), 40)
	src := jpegWithOrientation(raw, 6)
	got := normalize(t, src, 64)
	if bytes.Equal(got.Bytes, src) {
		t.Fatal("retained a rotated source verbatim")
	}
	if got.Width != 32 || got.Height != 64 {
		t.Fatalf("size = %dx%d, want 32x64", got.Width, got.Height)
	}
	assertStoredDimensions(t, got)
}

func TestMeasurePSNRIgnoresDecoderColourConversion(t *testing.T) {
	// A lossless round trip is exact, so measuring it must not report a finite
	// loss. This is the regression guard for measuring through a decoder that
	// forces every WebP to 4:2:0 YCbCr.
	img := imaging.Resize(photoNRGBA(320, 240), 160, 0, imaging.Lanczos)
	encoded, err := encodeWebP(img, DefaultOpts(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := measurePSNR(img, encoded); got != math.Inf(1) {
		t.Fatalf("psnr of a lossless round trip = %v, want +Inf", got)
	}
}

func assertMinPSNR(t *testing.T, src []byte, got Result, minimum float64) {
	t.Helper()
	ref, err := imaging.Decode(bytes.NewReader(src), imaging.AutoOrientation(true))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Bounds().Dx() != got.Width {
		ref = imaging.Resize(ref, got.Width, 0, imaging.Lanczos)
	}
	if v := measurePSNR(ref, got.Bytes); v < minimum {
		t.Fatalf("psnr = %.2f, want >= %.2f", v, minimum)
	}
}

// assertSizeCeiling pins the encoded size so a dependency upgrade cannot quietly
// worsen compression. Ceilings sit just above the measured size on purpose.
func assertSizeCeiling(t *testing.T, got Result, ceiling int) {
	t.Helper()
	if len(got.Bytes) > ceiling {
		t.Fatalf("encoded %d bytes exceeds ceiling %d", len(got.Bytes), ceiling)
	}
}

func assertStoredDimensions(t *testing.T, got Result) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != got.Width || cfg.Height != got.Height {
		t.Fatalf("stored image is %dx%d but Result reports %dx%d",
			cfg.Width, cfg.Height, got.Width, got.Height)
	}
}

func isReddish(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return r>>8 > 150 && g>>8 < 100 && b>>8 < 100
}

func photoJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	return encodeJPEG(t, photoNRGBA(w, h), 92)
}

func logoJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	return encodeJPEG(t, logoNRGBA(w, h), 95)
}

func smallFlat(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{30, 30, 30, 255}}, image.Point{}, draw.Src)
	return img
}

func gradientJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 180, A: 255})
		}
	}
	return encodeJPEG(t, img, 92)
}

func faceJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.NRGBA{210, 170, 140, 255}}, image.Point{}, draw.Src)
	cx, cy := w/2, h/2
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy < (w*w)/6 {
				img.SetNRGBA(x, y, color.NRGBA{220, 180, 150, 255})
			}
		}
	}
	eyeY := cy - h/8
	for _, ex := range []int{cx - w/6, cx + w/6} {
		for y := eyeY - 4; y <= eyeY+4; y++ {
			for x := ex - 4; x <= ex+4; x++ {
				img.SetNRGBA(x, y, color.NRGBA{40, 30, 25, 255})
			}
		}
	}
	return encodeJPEG(t, img, 90)
}

func logoNRGBA(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.NRGBA{255, 255, 255, 255}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(40, 40, w-40, 160), &image.Uniform{color.NRGBA{20, 20, 20, 255}}, image.Point{}, draw.Src)
	for y := h/2 - 16; y < h/2+16; y++ {
		for x := 80; x < w-80; x++ {
			if (x/24)%2 == 0 {
				img.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 255})
			}
		}
	}
	return img
}

func logoPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	return encodePNG(t, logoNRGBA(w, h))
}

func alphaPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(0)
			if x > w/3 && x < 2*w/3 {
				a = 255
			}
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: 40, B: 40, A: a})
		}
	}
	return encodePNG(t, img)
}

// photoNRGBA is a stand-in for a photograph: smooth tonal drift plus enough
// fine detail that it compresses like one, so the size ceilings above mean
// something. The noise comes from a fixed LCG, so fixtures stay deterministic.
func photoNRGBA(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	state := uint32(0x9e3779b9)
	next := func() uint32 {
		state = state*1664525 + 1013904223
		return state >> 24
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			n := int(next()) - 128
			img.SetNRGBA(x, y, color.NRGBA{
				R: clamp8(70 + (x*90)/w + n/3),
				G: clamp8(90 + (y*70)/h + n/4),
				B: clamp8(60 + ((x+y)*60)/(w+h) + n/5),
				A: 255,
			})
		}
	}
	return imaging.Blur(img, 0.4)
}

func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// orientationProbe marks the top-left corner red so a rotation can be detected
// from the stored pixels rather than inferred from the reported size.
func orientationProbe(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{200, 200, 200, 255}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, 0, w/4, h/4), &image.Uniform{color.RGBA{255, 0, 0, 255}}, image.Point{}, draw.Src)
	return img
}

func encodeJPEG(t *testing.T, img image.Image, q int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegWithOrientation(jpegData []byte, orientation uint16) []byte {
	if len(jpegData) < 2 || jpegData[0] != 0xFF || jpegData[1] != 0xD8 {
		panic("not jpeg")
	}
	app1 := exifAPP1(orientation)
	out := make([]byte, 0, 2+len(app1)+len(jpegData)-2)
	out = append(out, 0xFF, 0xD8)
	out = append(out, app1...)
	out = append(out, jpegData[2:]...)
	return out
}

func exifAPP1(orientation uint16) []byte {
	// APP1 / Exif little-endian IFD with a single Orientation tag.
	body := []byte("Exif\x00\x00II*\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	binary.LittleEndian.PutUint16(body[24:26], orientation)
	app1 := make([]byte, 4+len(body))
	app1[0], app1[1] = 0xFF, 0xE1
	binary.BigEndian.PutUint16(app1[2:4], uint16(2+len(body)))
	copy(app1[4:], body)
	return app1
}
