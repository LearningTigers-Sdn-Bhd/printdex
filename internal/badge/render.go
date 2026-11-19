// Package badge lays a ticket out on a badge and draws it as a 1-bit bitmap
// at the printer's own resolution, so nothing is resampled on the way to
// paper. Layout math is in points (72 per inch), origin bottom-left.
package badge

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Sienz16/printdex/internal/config"
	"github.com/Sienz16/printdex/internal/ticket"
	"github.com/go-fonts/liberation/liberationsansbold"
	"github.com/go-fonts/liberation/liberationsansregular"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

const (
	inch = 72.0 // points per inch

	// Printers are 1-bit: anything darker than this prints as a dot. High on
	// purpose so thin anti-aliased edges stay solid instead of dropping out.
	// Calibration knob: lower it if text prints too heavy on a given stock.
	inkThreshold = 220
)

// --- fonts: Liberation Sans, metric-compatible with Helvetica ---

type fontSet struct {
	f     *sfnt.Font
	upem  float64
	adv   map[rune]float64 // advance width as a fraction of the em
	faces map[[2]int]font.Face

	fbOnce sync.Once
	fb     *sfnt.Font // CJK fallback, see fallback.go
}

var (
	fonts    [2]*fontSet // regular, bold
	advMu    sync.Mutex
	renderMu sync.Mutex // faces are not safe for concurrent use
)

func init() {
	for i, ttf := range [][]byte{liberationsansregular.TTF, liberationsansbold.TTF} {
		f, err := opentype.Parse(ttf)
		if err != nil {
			panic(err)
		}
		fonts[i] = &fontSet{f: f, upem: float64(f.UnitsPerEm()), adv: map[rune]float64{}, faces: map[[2]int]font.Face{}}
	}
}

func fontFor(bold bool) *fontSet {
	if bold {
		return fonts[1]
	}
	return fonts[0]
}

// stringWidth is the unkerned width of text in points, like reportlab's.
func stringWidth(text string, bold bool, size float64) float64 {
	fs := fontFor(bold)
	advMu.Lock()
	defer advMu.Unlock()
	var buf sfnt.Buffer
	total := 0.0
	for _, r := range text {
		a, ok := fs.adv[r]
		if !ok {
			f := fs.pick(&buf, r, bold)
			upem := float64(f.UnitsPerEm())
			idx, _ := f.GlyphIndex(&buf, r)
			units, _ := f.GlyphAdvance(&buf, idx, fixed.I(int(upem)), font.HintingNone)
			a = float64(units) / 64 / upem
			fs.adv[r] = a
		}
		total += a
	}
	return total * size
}

// --- rendering ---

func faceFor(bold bool, size, dpi float64) font.Face {
	fs := fontFor(bold)
	key := [2]int{int(size * 64), int(dpi * 16)}
	if face, ok := fs.faces[key]; ok {
		return face
	}
	if len(fs.faces) > 256 {
		clear(fs.faces) // ponytail: crude bound; auto-fit yields a small set of sizes in practice
	}
	opts := &opentype.FaceOptions{Size: size, DPI: dpi, Hinting: font.HintingNone}
	face, _ := opentype.NewFace(fs.f, opts)
	if fb := fs.fallback(bold); fb != nil {
		fbFace, _ := opentype.NewFace(fb, opts)
		face = &fallbackFace{Face: face, fb: fbFace, lib: fs.f}
	}
	fs.faces[key] = face
	return face
}

// drawCentered draws line centered on cx with its baseline at y (pixels).
func drawCentered(img draw.Image, face font.Face, cx, y float64, line string) {
	d := font.Drawer{Dst: img, Src: image.Black, Face: face}
	width := float64(d.MeasureString(line)) / 64
	d.Dot = fixed.Point26_6{X: fixed.Int26_6((cx - width/2) * 64), Y: fixed.Int26_6(y * 64)}
	d.DrawString(line)
}

// drawQR draws data as whole-pixel modules inside the square at (x, y), so
// every module edge is crisp at the printer's resolution.
func drawQR(img *image.Gray, data string, x, y, side float64) {
	q, err := qrcode.New(data, qrcode.Medium)
	if err != nil {
		return
	}
	q.DisableBorder = true
	bits := q.Bitmap()
	const border = 2
	n := len(bits) + 2*border
	module := max(1, int(side)/n)
	x0 := int(x + (side-float64(module*n))/2)
	y0 := int(y + (side-float64(module*n))/2)
	for row, line := range bits {
		for col, on := range line {
			if on {
				px, py := x0+(col+border)*module, y0+(row+border)*module
				draw.Draw(img, image.Rect(px, py, px+module, py+module), image.Black, image.Point{}, draw.Src)
			}
		}
	}
}

func blank(w, h, dpi float64) *image.Gray {
	s := dpi / 72
	img := image.NewGray(image.Rect(0, 0, int(math.Round(w*s)), int(math.Round(h*s))))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	return img
}

// binarize turns every pixel pure black or pure white. A 1-bit printer would
// otherwise halftone the grey anti-aliased edges into visible dots.
func binarize(img *image.Gray) *image.Gray {
	for i, v := range img.Pix {
		if v < inkThreshold {
			img.Pix[i] = 0
		} else {
			img.Pix[i] = 255
		}
	}
	return img
}

// qrData is what the badge QR encodes: the layout's static text or URL
// ("{id}" becomes the ticket id), or the bare ticket id when none is set.
func qrData(t ticket.Ticket, l config.Layout) string {
	if l.QRStatic == "" {
		return t.ID
	}
	return strings.ReplaceAll(l.QRStatic, "{id}", t.ID)
}

// Render draws the ticket's badge at dpi as a black-and-white bitmap.
func Render(t ticket.Ticket, l config.Layout, dpi float64) *image.Gray {
	blocks, top, w, h := compose(t, l)
	s := dpi / 72 // pixels per point
	img := blank(w, h, dpi)
	renderMu.Lock()
	defer renderMu.Unlock()
	y := top // top edge of the next block, in points from the bottom
	for i, b := range blocks {
		if i > 0 {
			y -= gap(blocks[i-1], b)
		}
		// The editor's nudge moves only this element, never the ones after it.
		nudge := l.ElementOffsets[b.element]
		dx, dy := nudge.DX/25.4*inch, nudge.DY/25.4*inch
		if b.isQR {
			drawQR(img, qrData(t, l), ((w-b.size)/2+dx)*s, (h-y-dy)*s, b.size*s)
			if b.label != "" {
				drawCentered(img, faceFor(false, qrLabelSize, dpi), (w/2+dx)*s, (h-(y-b.size-qrLabelDrop)-dy)*s, b.label)
			}
			y -= b.height()
			continue
		}
		face := faceFor(b.bold, b.size, dpi)
		for _, line := range b.lines {
			y -= b.lineHeight
			drawCentered(img, face, (w/2+dx)*s, (h-y-dy)*s, line)
		}
	}
	return binarize(img)
}

// TestPage is a bordered page with a timestamp, for checking the printer.
func TestPage(l config.Layout, dpi float64) *image.Gray {
	w, h := l.Paper.WidthMM/25.4*inch, l.Paper.HeightMM/25.4*inch
	s := dpi / 72
	img := blank(w, h, dpi)
	renderMu.Lock()
	defer renderMu.Unlock()
	in, line := int(0.2*inch*s), max(2, int(s))
	r := img.Bounds().Inset(in)
	for _, edge := range []image.Rectangle{
		{Min: r.Min, Max: image.Pt(r.Max.X, r.Min.Y+line)}, {Min: image.Pt(r.Min.X, r.Max.Y-line), Max: r.Max},
		{Min: r.Min, Max: image.Pt(r.Min.X+line, r.Max.Y)}, {Min: image.Pt(r.Max.X-line, r.Min.Y), Max: r.Max},
	} {
		draw.Draw(img, edge, image.Black, image.Point{}, draw.Src)
	}
	drawCentered(img, faceFor(true, 14, dpi), w/2*s, 0.6*inch*s, "PrintDex — TEST")
	drawCentered(img, faceFor(false, 10, dpi), w/2*s, 1.0*inch*s, time.Now().Format("2006-01-02 15:04:05"))
	return binarize(img)
}

// Thicken grows dark strokes by a pixel on every side. Direct-thermal on
// rough stock drops dots; fatter strokes survive that.
func Thicken(src *image.Gray) *image.Gray {
	b := src.Bounds()
	dst := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			darkest := uint8(255)
			for _, p := range [9][2]int{{-1, -1}, {0, -1}, {1, -1}, {-1, 0}, {0, 0}, {1, 0}, {-1, 1}, {0, 1}, {1, 1}} {
				if q := image.Pt(x+p[0], y+p[1]); q.In(b) {
					darkest = min(darkest, src.GrayAt(q.X, q.Y).Y)
				}
			}
			dst.SetGray(x, y, color.Gray{Y: darkest})
		}
	}
	return dst
}

// PNG encodes a rendered bitmap as a 1-bit PNG tagged with its resolution,
// so CUPS prints it at true size.
func PNG(img *image.Gray, dpi float64) []byte {
	b := img.Bounds()
	bw := image.NewPaletted(b, color.Palette{color.White, color.Black})
	for i, v := range img.Pix {
		if v == 0 {
			bw.Pix[i] = 1
		}
	}
	var buf bytes.Buffer
	_ = (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, bw)
	out := buf.Bytes()

	// pHYs chunk (pixels per metre), inserted right after the 33-byte signature+IHDR.
	chunk := make([]byte, 21)
	binary.BigEndian.PutUint32(chunk, 9)
	copy(chunk[4:], "pHYs")
	ppm := uint32(math.Round(dpi / 0.0254))
	binary.BigEndian.PutUint32(chunk[8:], ppm)
	binary.BigEndian.PutUint32(chunk[12:], ppm)
	chunk[16] = 1
	binary.BigEndian.PutUint32(chunk[17:], crc32.ChecksumIEEE(chunk[4:17]))
	return append(out[:33:33], append(chunk, out[33:]...)...)
}
