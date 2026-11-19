package badge

import (
	_ "embed"
	"image"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// Liberation Sans has no CJK glyphs, so Chinese names drew as .notdef boxes.
// Any rune it lacks is drawn from an embedded Noto Sans SC subset (about 10k
// common simplified and traditional characters, see fonts/OFL.txt), so no font
// needs installing on the machine. ponytail: a rare name character outside the
// subset still prints as a box; widen the subset (pyftsubset) if one turns up.
var (
	//go:embed fonts/NotoSansSC-Regular.subset.otf
	cjkRegular []byte
	//go:embed fonts/NotoSansSC-Bold.subset.otf
	cjkBold []byte
)

// fallback is this weight's CJK font, parsed on first use.
func (fs *fontSet) fallback(bold bool) *sfnt.Font {
	fs.fbOnce.Do(func() {
		data := cjkRegular
		if bold {
			data = cjkBold
		}
		fs.fb, _ = opentype.Parse(data)
	})
	return fs.fb
}

// pick is the font that should draw r: Liberation, unless it lacks r and the
// fallback font is there.
func (fs *fontSet) pick(buf *sfnt.Buffer, r rune, bold bool) *sfnt.Font {
	if idx, _ := fs.f.GlyphIndex(buf, r); idx != 0 {
		return fs.f
	}
	if fb := fs.fallback(bold); fb != nil {
		return fb
	}
	return fs.f
}

// fallbackFace draws each rune with Liberation, or the CJK face when
// Liberation has no glyph for it. Not safe for concurrent use (renderMu).
type fallbackFace struct {
	font.Face // Liberation: Close, Kern, Metrics
	fb        font.Face
	lib       *sfnt.Font
	buf       sfnt.Buffer
}

func (f *fallbackFace) face(r rune) font.Face {
	if idx, _ := f.lib.GlyphIndex(&f.buf, r); idx == 0 {
		return f.fb
	}
	return f.Face
}

func (f *fallbackFace) Glyph(dot fixed.Point26_6, r rune) (image.Rectangle, image.Image, image.Point, fixed.Int26_6, bool) {
	return f.face(r).Glyph(dot, r)
}

func (f *fallbackFace) GlyphBounds(r rune) (fixed.Rectangle26_6, fixed.Int26_6, bool) {
	return f.face(r).GlyphBounds(r)
}

func (f *fallbackFace) GlyphAdvance(r rune) (fixed.Int26_6, bool) {
	return f.face(r).GlyphAdvance(r)
}
