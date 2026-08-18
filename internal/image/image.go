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
	_ "golang.org/x/image/webp"
)

type (
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
		MaxPixels              int
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
		MaxPixels:              50_000_000,
		MateriallySmallerRatio: 0.90,
		MinPSNR:                35,
	}
}

func Normalize(src []byte, requestedWidth int, opts Opts) (Result, error) {
	if requestedWidth <= 0 {
		return Result{}, ErrInvalidWidth
	}
	opts = withDefaults(opts)

	kind, err := filetype.Match(src)
	if err != nil {
		return Result{}, err
	}
	format := kind.Extension
	if format != "jpg" && format != "png" && format != "webp" {
		return Result{}, ErrUnsupported
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return Result{}, err
	}
	if opts.MaxPixels > 0 && int64(cfg.Width)*int64(cfg.Height) > int64(opts.MaxPixels) {
		return Result{}, ErrTooManyPixels
	}

	img, err := imaging.Decode(bytes.NewReader(src), imaging.AutoOrientation(true))
	if err != nil {
		return Result{}, err
	}

	srcW := img.Bounds().Dx()
	srcH := img.Bounds().Dy()
	outW := srcW
	if requestedWidth < srcW {
		outW = requestedWidth
	}
	resized := outW < srcW
	if resized {
		img = imaging.Resize(img, outW, 0, imaging.Lanczos)
	}
	outH := img.Bounds().Dy()

	original := retained(src, format, srcW, srcH)
	if format == "webp" && !resized {
		return original, nil
	}

	hasAlpha := imageHasAlpha(img)
	useLossless := hasAlpha || format == "png" || (format != "jpg" && isGraphic(img))

	if useLossless {
		encoded, err := encodeWebP(img, opts, true, hasAlpha)
		if err != nil {
			return Result{}, err
		}
		if !resized && !materiallySmaller(encoded, src, opts.MateriallySmallerRatio) {
			return original, nil
		}
		return webpResult(encoded, outW, outH, true), nil
	}

	encoded, err := encodeWebP(img, opts, false, false)
	if err != nil {
		return Result{}, err
	}
	if !qualityOK(img, encoded, opts) {
		lossless, err := encodeWebP(img, opts, true, hasAlpha)
		if err != nil {
			return Result{}, err
		}
		if !resized && !materiallySmaller(lossless, src, opts.MateriallySmallerRatio) {
			return original, nil
		}
		return webpResult(lossless, outW, outH, true), nil
	}
	if !resized && !materiallySmaller(encoded, src, opts.MateriallySmallerRatio) {
		return original, nil
	}
	return webpResult(encoded, outW, outH, false), nil
}

func withDefaults(o Opts) Opts {
	d := DefaultOpts()
	if o.Quality <= 0 {
		o.Quality = d.Quality
	}
	if o.Method <= 0 {
		o.Method = d.Method
	}
	if o.MaxPixels == 0 {
		o.MaxPixels = d.MaxPixels
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

func qualityOK(src image.Image, encoded []byte, opts Opts) bool {
	got, err := webp.Decode(bytes.NewReader(encoded))
	if err != nil {
		return false
	}
	refBytes, err := encodeWebP(src, Opts{Method: 4, Quality: 100}, true, false)
	if err != nil {
		return false
	}
	ref, err := webp.Decode(bytes.NewReader(refBytes))
	if err != nil {
		return false
	}
	return psnr(ref, got) >= opts.MinPSNR
}

func psnr(a, b image.Image) float64 {
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return 0
	}
	var sum float64
	pixels := ab.Dx() * ab.Dy()
	if pixels == 0 {
		return 0
	}
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
	switch img.(type) {
	case *image.NRGBA, *image.NRGBA64, *image.RGBA, *image.RGBA64, *image.NYCbCrA:
	default:
		return false
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a < 0xffff {
				return true
			}
		}
	}
	return false
}

func isGraphic(img image.Image) bool {
	b := img.Bounds()
	if b.Dx() < 2 {
		return false
	}
	var flat, total int
	for y := b.Min.Y; y < b.Max.Y; y++ {
		pr, pg, pb, _ := img.At(b.Min.X, y).RGBA()
		for x := b.Min.X + 1; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r == pr && g == pg && bl == pb {
				flat++
			}
			pr, pg, pb = r, g, bl
			total++
		}
	}
	return total > 0 && float64(flat)/float64(total) >= 0.40
}
