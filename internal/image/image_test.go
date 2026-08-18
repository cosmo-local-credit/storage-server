package image

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/disintegration/imaging"
	"github.com/gen2brain/webp"
)

func TestNormalizeRejectsInvalidWidth(t *testing.T) {
	_, err := Normalize(photoJPEG(t, 32, 24), 0, DefaultOpts())
	if err != ErrInvalidWidth {
		t.Fatalf("err = %v, want ErrInvalidWidth", err)
	}
}

func TestNormalizeRejectsOversizedPixels(t *testing.T) {
	src := photoJPEG(t, 16, 16)
	_, err := Normalize(src, 16, Opts{MaxPixels: 10})
	if err != ErrTooManyPixels {
		t.Fatalf("err = %v, want ErrTooManyPixels", err)
	}
}

func TestNormalizeRejectsUnsupported(t *testing.T) {
	_, err := Normalize([]byte("%PDF-1.1 not an image"), 400, DefaultOpts())
	if err != ErrUnsupported {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestNormalizeNeverUpscales(t *testing.T) {
	src := photoJPEG(t, 320, 200)
	got, err := Normalize(src, 800, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 320 || got.Height != 200 {
		t.Fatalf("size = %dx%d, want 320x200", got.Width, got.Height)
	}
}

func TestNormalizeResizedJPEGIsLossyWebP(t *testing.T) {
	src := photoJPEG(t, 800, 500)
	got, err := Normalize(src, 400, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 400 {
		t.Fatalf("width = %d, want 400", got.Width)
	}
	if got.Height != 250 {
		t.Fatalf("height = %d, want 250", got.Height)
	}
	if got.Extension != "webp" || got.ContentType != "image/webp" {
		t.Fatalf("format = %s %s", got.Extension, got.ContentType)
	}
	if got.Lossless {
		t.Fatal("expected lossy webp for resized jpeg")
	}
	if len(got.Bytes) >= len(src) {
		t.Fatalf("encoded %d >= original %d", len(got.Bytes), len(src))
	}
	if len(got.Bytes) > 80_000 {
		t.Fatalf("encoded %d exceeds size ceiling", len(got.Bytes))
	}
	assertPSNR(t, mustJPEG(t, src), got, 35)
}

func TestNormalizeLogoAndTextAreLossless(t *testing.T) {
	src := logoPNG(t, 600, 400)
	got, err := Normalize(src, 400, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Lossless || got.Extension != "webp" {
		t.Fatalf("lossless=%v ext=%s, want lossless webp", got.Lossless, got.Extension)
	}
	if got.Width > 400 || got.Height > 400 {
		t.Fatalf("logo size %dx%d exceeds requested bounds", got.Width, got.Height)
	}
}

func TestNormalizePreservesAlpha(t *testing.T) {
	src := alphaPNG(t, 80, 60)
	got, err := Normalize(src, 40, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Lossless {
		t.Fatal("alpha input must stay lossless")
	}
	decoded, err := imaging.Decode(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if !imageHasAlpha(decoded) {
		t.Fatal("alpha channel was dropped")
	}
}

func TestNormalizeKeepsAlreadyWebP(t *testing.T) {
	img := photoNRGBA(240, 160)
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, webp.Options{Quality: 82, Method: 6}); err != nil {
		t.Fatal(err)
	}
	src := buf.Bytes()
	got, err := Normalize(src, 240, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes, src) {
		t.Fatalf("re-encoded already-normalised webp (%d vs %d bytes)", len(got.Bytes), len(src))
	}
	if got.Extension != "webp" {
		t.Fatalf("ext = %s", got.Extension)
	}
}

func TestNormalizeSameSizeJPEGKeepsOriginalWhenNotWorthIt(t *testing.T) {
	src := smallJPEG(t, 64, 48)
	got, err := Normalize(src, 64, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	if got.Width > 64 {
		t.Fatalf("width %d exceeds source", got.Width)
	}
	if got.Extension == "webp" && len(got.Bytes) > int(float64(len(src))*0.90) {
		t.Fatalf("converted without a material saving: %d vs %d", len(got.Bytes), len(src))
	}
}

func TestNormalizeEXIFOrientations(t *testing.T) {
	base := distinctiveRGBA(40, 20)
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, base, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}

	for o := 1; o <= 8; o++ {
		src := jpegWithOrientation(raw.Bytes(), uint16(o))
		got, err := Normalize(src, 40, DefaultOpts())
		if err != nil {
			t.Fatalf("orientation %d: %v", o, err)
		}
		wantW, wantH := 40, 20
		if o >= 5 {
			wantW, wantH = 20, 40
		}
		if got.Width != wantW || got.Height != wantH {
			t.Fatalf("orientation %d size = %dx%d, want %dx%d", o, got.Width, got.Height, wantW, wantH)
		}
	}
}

func TestNormalizeOrientation6IsPortrait(t *testing.T) {
	base := distinctiveRGBA(40, 20)
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, base, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	got, err := Normalize(jpegWithOrientation(raw.Bytes(), 6), 40, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	if got.Width >= got.Height {
		t.Fatalf("orientation 6 should be portrait, got %dx%d", got.Width, got.Height)
	}
}

func TestNormalizeFacesAndGradientsMeetQualityFloor(t *testing.T) {
	for _, src := range [][]byte{
		gradientJPEG(t, 400, 240),
		faceJPEG(t, 320, 320),
	} {
		got, err := Normalize(src, 200, DefaultOpts())
		if err != nil {
			t.Fatal(err)
		}
		if got.Lossless {
			continue
		}
		assertPSNR(t, mustJPEG(t, src), got, 35)
		if len(got.Bytes) > 60_000 {
			t.Fatalf("encoded %d exceeds size ceiling", len(got.Bytes))
		}
	}
}

func TestNormalizeOutputNeverExceedsBounds(t *testing.T) {
	src := photoJPEG(t, 640, 400)
	for _, width := range []int{400, 800, 1280} {
		got, err := Normalize(src, width, DefaultOpts())
		if err != nil {
			t.Fatal(err)
		}
		if got.Width > 640 || got.Width > width {
			t.Fatalf("width %d exceeds min(source=640, requested=%d)", got.Width, width)
		}
	}
}

func assertPSNR(t *testing.T, src image.Image, got Result, min float64) {
	t.Helper()
	decoded, err := webp.Decode(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	refImg := src
	if src.Bounds().Dx() != got.Width {
		refImg = imaging.Resize(src, got.Width, 0, imaging.Lanczos)
	}
	refBytes, err := encodeWebP(refImg, Opts{Method: 4, Quality: 100}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := webp.Decode(bytes.NewReader(refBytes))
	if err != nil {
		t.Fatal(err)
	}
	if v := psnr(ref, decoded); v < min {
		t.Fatalf("psnr = %.2f, want >= %.2f", v, min)
	}
}

func mustJPEG(t *testing.T, src []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func photoJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	return encodeJPEG(t, photoNRGBA(w, h), 92)
}

func smallJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{30, 30, 30, 255}}, image.Point{}, draw.Src)
	return encodeJPEG(t, img, 40)
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

func logoPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.NRGBA{255, 255, 255, 255}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(20, 20, w-20, 80), &image.Uniform{color.NRGBA{20, 20, 20, 255}}, image.Point{}, draw.Src)
	for y := h/2 - 8; y < h/2+8; y++ {
		for x := 40; x < w-40; x++ {
			if (x/12)%2 == 0 {
				img.SetNRGBA(x, y, color.NRGBA{0, 0, 0, 255})
			}
		}
	}
	return encodePNG(t, img)
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

func photoNRGBA(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(70 + (x*50)/w + (x*y)%17),
				G: uint8(90 + (y*40)/h + (x+y)%13),
				B: uint8(60 + ((x+y)*25)/(w+h) + (y*3)%11),
				A: 255,
			})
		}
	}
	return imaging.Blur(img, 1.2)
}

func distinctiveRGBA(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{200, 200, 200, 255}}, image.Point{}, draw.Src)
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	img.Set(w-1, 0, color.RGBA{0, 0, 255, 255})
	img.Set(0, h-1, color.RGBA{0, 255, 0, 255})
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
