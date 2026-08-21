package image

import (
	"bytes"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"

	"github.com/disintegration/imaging"
	"github.com/deepteams/webp"
	xwebp "golang.org/x/image/webp"
)

type (
	// Info is an upload's container and dimensions as they appear on the wire,
	// before EXIF orientation is applied.
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

// extensions are the accepted formats, keyed by the name the registered decoders
// report.
var extensions = map[string]string{
	"jpeg": "jpg",
	"png":  "png",
	"webp": "webp",
}

func DefaultOpts() Opts {
	return Opts{
		Quality:                82,
		Method:                 6,
		MateriallySmallerRatio: 0.90,
		MinPSNR:                30,
	}
}

// Inspect reads the container and dimensions from the header without allocating a
// pixel buffer. Normalize takes the Info it returns, so an unreadable or
// oversized upload is rejected before it is decoded.
func Inspect(src []byte, maxPixels int) (Info, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return Info{}, ErrUnsupported
	}
	ext, ok := extensions[format]
	if !ok {
		return Info{}, ErrUnsupported
	}
	if maxPixels > 0 && int64(cfg.Width)*int64(cfg.Height) > int64(maxPixels) {
		return Info{}, ErrTooManyPixels
	}
	return Info{Format: ext, Width: cfg.Width, Height: cfg.Height}, nil
}

// Normalize decodes src once and produces one output at
// min(sourceWidth, requestedWidth), preserving the aspect ratio.
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

	// A quarter-turn EXIF rotation leaves the stored bytes describing a different
	// frame than the caller will see, so those are re-encoded rather than kept.
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

// encode produces the one delivery encoding for img.
//
// Alpha has to survive, so anything carrying it is lossless. Everything else is
// encoded lossy and measured; only output that misses the floor is also encoded
// lossless, and then the smaller wins. Lossless WebP beats lossy on size exactly
// for the flat, hard-edged content lossy handles worst, which separates logos and
// text from photographs without guessing from the container.
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
	if err := webp.Encode(&buf, img, &webp.EncoderOptions{
		Quality:  float32(opts.Quality),
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

// measurePSNR compares the candidate against the pixels it was made from.
// Decoding goes through x/image/webp because it returns lossless WebP as RGBA and
// lossy as the YCbCr the file stores. A decoder that forces 4:2:0 on lossless
// would inflate the result by tens of decibels.
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

func imageHasAlpha(img image.Image) bool {
	o, ok := img.(interface{ Opaque() bool })
	return ok && !o.Opaque()
}
