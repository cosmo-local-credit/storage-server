package image

import (
	"bytes"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"

	"github.com/disintegration/imaging"
	"github.com/gen2brain/webp"
	"github.com/h2non/filetype"
	xwebp "golang.org/x/image/webp"
)

type (
	// Info describes an upload's container and pixel dimensions as they appear on
	// the wire, before any EXIF orientation is applied.
	Info struct {
		Format string
		Width  int
		Height int
	}

	Result struct {
		Width       int
		Height      int
		Bytes       []byte
		ContentType string
		Extension   string
		Lossless    bool
	}

	Opts struct {
		Quality                int
		Method                 int
		MateriallySmallerRatio float64
		MinPSNR                float64
	}
)

var (
	ErrUnsupported   = errors.New("unsupported image type")
	ErrTooManyPixels = errors.New("image exceeds max pixels")
	ErrInvalidWidth  = errors.New("invalid requested width")
)

func DefaultOpts() Opts {
	return Opts{
		Quality:                82,
		Method:                 6,
		MateriallySmallerRatio: 0.90,
		MinPSNR:                30,
	}
}

// Inspect validates the container and reads the dimensions from the header
// without allocating a pixel buffer. Normalize takes the Info it returns, so an
// oversized or unsupported upload is always rejected before it is decoded.
func Inspect(src []byte, maxPixels int) (Info, error) {
	kind, err := filetype.Match(src)
	if err != nil {
		return Info{}, err
	}
	switch kind.Extension {
	case "jpg", "png", "webp":
	default:
		return Info{}, ErrUnsupported
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return Info{}, err
	}
	if maxPixels > 0 && int64(cfg.Width)*int64(cfg.Height) > int64(maxPixels) {
		return Info{}, ErrTooManyPixels
	}

	return Info{Format: kind.Extension, Width: cfg.Width, Height: cfg.Height}, nil
}

// Normalize decodes src once and produces exactly one output at
// min(sourceWidth, requestedWidth), preserving the aspect ratio. info must come
// from Inspect.
func Normalize(src []byte, info Info, requestedWidth int, opts Opts) (Result, error) {
	if requestedWidth <= 0 {
		return Result{}, ErrInvalidWidth
	}
	if info.Format == "" {
		return Result{}, ErrUnsupported
	}
	opts = withDefaults(opts)

	img, err := imaging.Decode(bytes.NewReader(src), imaging.AutoOrientation(true))
	if err != nil {
		return Result{}, err
	}

	srcW, srcH := img.Bounds().Dx(), img.Bounds().Dy()
	outW := min(srcW, requestedWidth)
	if outW < srcW {
		img = imaging.Resize(img, outW, 0, imaging.Lanczos)
	}
	outH := img.Bounds().Dy()

	// The source bytes may only be handed back untouched when nothing about the
	// image changed. A quarter-turn EXIF rotation leaves the stored bytes
	// describing a different frame than the caller will see, so those are always
	// re-encoded upright rather than retained with their orientation tag.
	retainable := outW == srcW && srcW == info.Width && srcH == info.Height

	if info.Format == "webp" && retainable {
		return retained(src, info.Format, srcW, srcH), nil
	}

	candidate, err := encode(img, outW, outH, opts)
	if err != nil {
		return Result{}, err
	}
	if retainable && !materiallySmaller(candidate.Bytes, src, opts.MateriallySmallerRatio) {
		return retained(src, info.Format, srcW, srcH), nil
	}
	return candidate, nil
}

// encode produces the single delivery encoding for img.
//
// Transparency has to survive, so anything carrying alpha is lossless. Anything
// else is encoded lossy first and then measured: output that clears the quality
// floor ships as it is. Only output that does not is also encoded lossless, and
// then the smaller of the two wins. Lossless WebP beats lossy WebP on size
// precisely for the flat, hard-edged content that lossy handles badly, so that
// comparison separates logos and text from photographs without having to guess
// at the subject from the container or from pixel statistics.
func encode(img image.Image, w, h int, opts Opts) (Result, error) {
	if imageHasAlpha(img) {
		encoded, err := encodeWebP(img, opts, true, true)
		if err != nil {
			return Result{}, err
		}
		return webpResult(encoded, w, h, true), nil
	}

	lossy, err := encodeWebP(img, opts, false, false)
	if err != nil {
		return Result{}, err
	}
	if measurePSNR(img, lossy) >= opts.MinPSNR {
		return webpResult(lossy, w, h, false), nil
	}

	lossless, err := encodeWebP(img, opts, true, false)
	if err != nil {
		return Result{}, err
	}
	if len(lossless) < len(lossy) {
		return webpResult(lossless, w, h, true), nil
	}
	return webpResult(lossy, w, h, false), nil
}

func withDefaults(o Opts) Opts {
	d := DefaultOpts()
	if o.Quality <= 0 {
		o.Quality = d.Quality
	}
	if o.Method <= 0 {
		o.Method = d.Method
	}
	if o.MateriallySmallerRatio <= 0 {
		o.MateriallySmallerRatio = d.MateriallySmallerRatio
	}
	if o.MinPSNR <= 0 {
		o.MinPSNR = d.MinPSNR
	}
	return o
}

func retained(src []byte, format string, w, h int) Result {
	switch format {
	case "png":
		return Result{Width: w, Height: h, Bytes: src, ContentType: "image/png", Extension: "png", Lossless: true}
	case "webp":
		return Result{Width: w, Height: h, Bytes: src, ContentType: "image/webp", Extension: "webp"}
	default:
		return Result{Width: w, Height: h, Bytes: src, ContentType: "image/jpeg", Extension: "jpg"}
	}
}

func webpResult(b []byte, w, h int, lossless bool) Result {
	return Result{
		Width:       w,
		Height:      h,
		Bytes:       b,
		ContentType: "image/webp",
		Extension:   "webp",
		Lossless:    lossless,
	}
}

func encodeWebP(img image.Image, opts Opts, lossless, exact bool) ([]byte, error) {
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, webp.Options{
		Quality:  opts.Quality,
		Method:   opts.Method,
		Lossless: lossless,
		Exact:    exact,
	}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func materiallySmaller(candidate, original []byte, ratio float64) bool {
	return float64(len(candidate)) <= float64(len(original))*ratio
}

// measurePSNR compares the encoded candidate against the pixels it was made
// from. Decoding goes through x/image/webp because it returns lossless WebP as
// RGBA and lossy WebP as the YCbCr the file actually stores; a decoder that
// forces every WebP to 4:2:0 YCbCr would charge the candidate for a conversion
// its bytes do not contain and inflate the result by tens of decibels.
func measurePSNR(src image.Image, encoded []byte) float64 {
	got, err := xwebp.Decode(bytes.NewReader(encoded))
	if err != nil {
		return 0
	}
	return psnr(src, got)
}

func psnr(a, b image.Image) float64 {
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return 0
	}
	pixels := ab.Dx() * ab.Dy()
	if pixels == 0 {
		return 0
	}
	var sum float64
	for y := 0; y < ab.Dy(); y++ {
		for x := 0; x < ab.Dx(); x++ {
			r1, g1, b1, _ := a.At(ab.Min.X+x, ab.Min.Y+y).RGBA()
			r2, g2, b2, _ := b.At(bb.Min.X+x, bb.Min.Y+y).RGBA()
			dr := float64(int(r1>>8) - int(r2>>8))
			dg := float64(int(g1>>8) - int(g2>>8))
			db := float64(int(b1>>8) - int(b2>>8))
			sum += dr*dr + dg*dg + db*db
		}
	}
	if sum == 0 {
		return math.Inf(1)
	}
	mse := sum / float64(pixels*3)
	return 10 * math.Log10((255*255)/mse)
}

// imageHasAlpha reports whether img carries a non-opaque pixel. Types with no
// alpha channel answer immediately, and the packed 8-bit types are scanned over
// their pixel slice: At() on a 12 MP frame is millions of interface calls.
func imageHasAlpha(img image.Image) bool {
	switch m := img.(type) {
	case *image.NRGBA:
		return anyTransparent(m.Pix, m.Stride, m.Rect.Dx(), m.Rect.Dy(), 4)
	case *image.RGBA:
		return anyTransparent(m.Pix, m.Stride, m.Rect.Dx(), m.Rect.Dy(), 4)
	case *image.NRGBA64, *image.RGBA64, *image.NYCbCrA:
		return anyTransparentAt(img)
	default:
		return false
	}
}

func anyTransparent(pix []byte, stride, w, h, pixSize int) bool {
	for y := 0; y < h; y++ {
		row := pix[y*stride : y*stride+w*pixSize]
		for x := pixSize - 1; x < len(row); x += pixSize {
			if row[x] != 0xff {
				return true
			}
		}
	}
	return false
}

func anyTransparentAt(img image.Image) bool {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xffff {
				return true
			}
		}
	}
	return false
}
