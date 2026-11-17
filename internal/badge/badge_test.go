package badge

import (
	"bytes"
	"image"
	"image/png"
	"testing"

	"github.com/Sienz16/printdex/internal/config"
	"github.com/Sienz16/printdex/internal/ticket"
)

var guest = ticket.Ticket{ID: "A1-0245", Name: "Mohamed Rafi Bin Abdullah", Company: "Learning Tigers Sdn Bhd", Type: "Conference Pass"}

func TestComposeKeepsHierarchyAndStaysOnPaper(t *testing.T) {
	blocks, top, _, height := compose(guest, config.DefaultLayout())
	if len(blocks) != 4 {
		t.Fatalf("want name, role, company, qr; got %d blocks", len(blocks))
	}
	name, role, company := blocks[0], blocks[1], blocks[2]
	if name.size <= role.size || role.size <= company.size {
		t.Errorf("hierarchy broken: name %.1f, role %.1f, company %.1f", name.size, role.size, company.size)
	}
	if role.lines[0] != "DELEGATE" {
		t.Errorf("conference pass should read DELEGATE, got %q", role.lines[0])
	}
	if top > height || top-stackHeight(blocks) < bottomMargin-0.01 {
		t.Errorf("stack leaves the paper: top %.1f, stack %.1f, paper %.1f", top, stackHeight(blocks), height)
	}
}

func TestSparseBadgeGrowsButRespectsCaps(t *testing.T) {
	layout := config.DefaultLayout()
	layout.Elements = []string{"name", "role"}
	blocks, _, _, _ := compose(ticket.Ticket{ID: "1", Name: "Li Wei", Type: "VVIP"}, layout)
	if blocks[0].size <= nameSizes[0] {
		t.Errorf("name did not grow past its natural %.0fpt: %.1f", nameSizes[0], blocks[0].size)
	}
	for _, b := range blocks {
		if b.size > maxGrownSize(b.element) {
			t.Errorf("%s grew past its cap: %.1f", b.element, b.size)
		}
	}
}

func TestCrowdedBadgeShrinksToItsFloors(t *testing.T) {
	layout := config.DefaultLayout()
	layout.Paper = config.Paper{WidthMM: 50, HeightMM: 30}
	layout.Elements = []string{"name", "role", "company", "title", "country", "qr"}
	crowded := ticket.Ticket{
		ID: "X", Type: "Delegate", Name: "Dr Maximilian Alexander von Hohenzollern-Sigmaringen the Third",
		Company: "International Consortium for Sustainable Infrastructure Development",
		Title:   "Chief Executive Officer", Country: "Federal Republic of Germany",
	}
	blocks, _, _, height := compose(crowded, layout)
	overflowing := stackHeight(blocks) > height
	for _, b := range blocks {
		floor := minTextSize
		if b.isQR {
			floor = qrMinSize
		}
		if b.size < floor-0.01 {
			t.Errorf("%s shrank below its floor: %.1f", b.element, b.size)
		}
		if overflowing && b.size > floor {
			t.Errorf("badge overflows but %s is still above its floor", b.element)
		}
	}
}

func TestRenderIsBlackAndWhiteAtPrintSize(t *testing.T) {
	page := Render(guest, config.DefaultLayout(), 203)
	if b := page.Bounds(); b.Dx() != 799 || b.Dy() != 639 { // 100x80mm at 203dpi
		t.Errorf("size = %dx%d", b.Dx(), b.Dy())
	}
	ink := 0
	for _, v := range page.Pix {
		switch v {
		case 0:
			ink++
		case 255:
		default:
			t.Fatalf("grey pixel %d: a 1-bit printer would dither it", v)
		}
	}
	if ink < 5000 {
		t.Errorf("badge is nearly blank: %d black pixels", ink)
	}
	decoded, err := png.Decode(bytes.NewReader(PNG(page, 203)))
	if err != nil {
		t.Fatalf("PNG with resolution chunk does not decode: %v", err)
	}
	if decoded.Bounds() != page.Bounds() {
		t.Errorf("PNG size %v, want %v", decoded.Bounds(), page.Bounds())
	}
}

func TestThickenGrowsInk(t *testing.T) {
	dot := image.NewGray(image.Rect(0, 0, 5, 5))
	for i := range dot.Pix {
		dot.Pix[i] = 255
	}
	dot.Pix[12] = 0 // centre pixel
	ink := 0
	for _, v := range Thicken(dot).Pix {
		if v == 0 {
			ink++
		}
	}
	if ink != 9 {
		t.Errorf("one dot should become a 3x3 block, got %d pixels", ink)
	}
}

func BenchmarkRenderAndEncode(b *testing.B) {
	layout := config.DefaultLayout()
	for b.Loop() {
		PNG(Render(guest, layout, 203), 203)
	}
}
