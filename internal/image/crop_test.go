package image

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"testing"

	xwebp "golang.org/x/image/webp"
)

func TestCentreRect(t *testing.T) {
	for name, tc := range map[string]struct {
		w, h   int
		aspect Aspect
		want   image.Rectangle
	}{
		"landscape to square":    {1000, 500, Aspect{1, 1}, image.Rect(250, 0, 750, 500)},
		"portrait to square":     {500, 1000, Aspect{1, 1}, image.Rect(0, 250, 500, 750)},
		"already square":         {640, 640, Aspect{1, 1}, image.Rect(0, 0, 640, 640)},
		"already 16:9":           {1600, 900, Aspect{16, 9}, image.Rect(0, 0, 1600, 900)},
		"square to 16:9":         {1000, 1000, Aspect{16, 9}, image.Rect(0, 218, 1000, 781)},
		"too wide for 16:9":      {2000, 900, Aspect{16, 9}, image.Rect(200, 0, 1800, 900)},
		"small square to 16:9":   {300, 300, Aspect{16, 9}, image.Rect(0, 65, 300, 234)},
		"odd remainder goes far": {5, 4, Aspect{1, 1}, image.Rect(0, 0, 4, 4)},
		"one pixel never empty":  {1, 1, Aspect{16, 9}, image.Rect(0, 0, 1, 1)},
	} {
		t.Run(name, func(t *testing.T) {
			got := CentreRect(tc.w, tc.h, tc.aspect)
			if got != tc.want {
				t.Fatalf("CentreRect(%d, %d, %v) = %v, want %v", tc.w, tc.h, tc.aspect, got, tc.want)
			}
			if !got.In(image.Rect(0, 0, tc.w, tc.h)) {
				t.Fatalf("%v leaves the %dx%d frame", got, tc.w, tc.h)
			}
		})
	}
}

func TestNormalizeCroppedProducesTheAspect(t *testing.T) {
	for name, tc := range map[string]struct {
		w, h         int
		aspect       Aspect
		width        int
		wantW, wantH int
	}{
		"landscape square": {1200, 800, Aspect{1, 1}, 400, 400, 400},
		"portrait square":  {800, 1200, Aspect{1, 1}, 800, 800, 800},
		"square to 16:9":   {1000, 1000, Aspect{16, 9}, 400, 400, 225},
		"wide to 16:9":     {2400, 900, Aspect{16, 9}, 1280, 1280, 720},
		// Never upscaled: the crop's own width stands when it is the narrower.
		"narrow source": {300, 300, Aspect{16, 9}, 400, 300, 169},
	} {
		t.Run(name, func(t *testing.T) {
			src := photoJPEG(t, tc.w, tc.h)
			info, err := Inspect(src, testMaxPixels)
			if err != nil {
				t.Fatal(err)
			}
			got, err := NormalizeCropped(src, info, tc.width, tc.aspect, DefaultOpts())
			if err != nil {
				t.Fatal(err)
			}
			if got.Width != tc.wantW || got.Height != tc.wantH {
				t.Fatalf("output = %dx%d, want %dx%d", got.Width, got.Height, tc.wantW, tc.wantH)
			}
			assertStoredDimensions(t, got)
		})
	}
}

// The crop keeps the middle of the frame: with red outer quarters and a green
// centre, a square crop of a 2:1 source sees no red at its edges.
func TestNormalizeCroppedKeepsTheCentre(t *testing.T) {
	const w, h = 1200, 600
	frame := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(frame, frame.Bounds(), &image.Uniform{color.NRGBA{200, 20, 20, 255}}, image.Point{}, draw.Src)
	draw.Draw(frame, image.Rect(w/4, 0, 3*w/4, h), &image.Uniform{color.NRGBA{20, 200, 20, 255}}, image.Point{}, draw.Src)
	src := encodePNG(t, frame)

	info, err := Inspect(src, testMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NormalizeCropped(src, info, 400, Aspect{1, 1}, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := xwebp.Decode(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	b := decoded.Bounds()
	for _, p := range []image.Point{{b.Min.X + 2, b.Min.Y + 2}, {b.Max.X - 3, b.Max.Y - 3}, {b.Min.X + 2, b.Max.Y - 3}} {
		r, g, _, _ := decoded.At(p.X, p.Y).RGBA()
		if r>>8 > 100 || g>>8 < 150 {
			t.Fatalf("pixel %v = (%d, %d), want the green centre", p, r>>8, g>>8)
		}
	}
}

// A crop changes the frame, so the source bytes are never stored in its place,
// even when re-encoding would not have been materially smaller.
func TestNormalizeCroppedIsNeverRetained(t *testing.T) {
	src := photoJPEG(t, 800, 400)
	info, err := Inspect(src, testMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NormalizeCropped(src, info, 1280, Aspect{1, 1}, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got.Bytes, src) || got.Width != 400 || got.Height != 400 {
		t.Fatalf("got %s %dx%d, want a re-encoded 400x400 crop", got.Extension, got.Width, got.Height)
	}
}

// The crop is taken from the frame the viewer sees. A 600x400 JPEG tagged
// orientation 6 is a 400x600 portrait, so 16:9 at 400 trims its height.
func TestNormalizeCroppedAfterOrientation(t *testing.T) {
	src := jpegWithOrientation(photoJPEG(t, 600, 400), 6)
	info, err := Inspect(src, testMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NormalizeCropped(src, info, 400, Aspect{16, 9}, DefaultOpts())
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 400 || got.Height != 225 {
		t.Fatalf("output = %dx%d, want 400x225", got.Width, got.Height)
	}
}

func TestNormalizeCroppedRejectsInvalidAspect(t *testing.T) {
	src := photoJPEG(t, 64, 64)
	info, err := Inspect(src, testMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []Aspect{{}, {0, 9}, {16, 0}, {-1, 1}} {
		if _, err := NormalizeCropped(src, info, 400, a, DefaultOpts()); !errors.Is(err, ErrInvalidAspect) {
			t.Fatalf("aspect %v: err = %v, want ErrInvalidAspect", a, err)
		}
	}
}
