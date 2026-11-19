package badge

import (
	"math"
	"strings"

	"github.com/Sienz16/printdex/internal/config"
	"github.com/Sienz16/printdex/internal/ticket"
)

// Layout works in three passes:
//
//  1. measure  each element gets its natural size and line breaks
//  2. grow     a sparse badge scales its text up to fill the paper
//  3. shrink   a crowded badge scales the QR, then the text, down to fit
//
// The finished stack of blocks is then centered on the paper.

const (
	sideMargin   = 0.2 * inch
	bottomMargin = 0.06 * inch
	edgeSafety   = 0.03 * inch // keeps glyph edges off the paper edge
	leading      = 3.0         // extra points between lines of text

	textGap = 0.07 * inch // between two text blocks
	qrGap   = 0.12 * inch // around a QR code

	qrNaturalSize = 1.15 * inch
	qrMinSize     = 0.55 * inch          // smaller than this stops scanning reliably
	qrLabelSize   = 8.0                  // caption under a QR code
	qrLabelDrop   = 0.75*qrLabelSize + 1 // QR bottom to caption baseline; the code's own quiet zone is the gap
	minTextSize   = 8.0

	// Growing stops when the stack fills this share of the paper height, and
	// no grown line may use more than this share of the text width.
	growFillTarget = 0.80
	growWidthLimit = 0.92
	growStep       = 0.05
)

// Text sizes in points, tried largest first.
var (
	nameSizes     = []float64{26, 24, 22, 20, 18, 16, 14, 12}
	headlineSizes = []float64{22, 20, 18, 16, 14, 12} // ticket type, table number
)

// maxGrownSize caps how far growing can take each element, so a two-field
// badge doesn't print "VVIP" at poster size and the name stays the largest
// thing on the badge.
func maxGrownSize(element string) float64 {
	switch element {
	case "name":
		return 44
	case "role":
		return 36
	case "table_no":
		return 30
	}
	return 20
}

// block is one element placed on the badge: a QR code or lines of text.
type block struct {
	element    string
	isQR       bool
	bold       bool
	size       float64  // font size, or the QR's side, in points
	lines      []string // text only
	lineHeight float64  // text only
	scale      float64  // the layout's size multiplier for this element
	label      string   // QR only: caption printed under the code
}

func (b block) height() float64 {
	if b.isQR {
		if b.label != "" {
			return b.size + qrLabelSize + 1
		}
		return b.size
	}
	return float64(len(b.lines)) * b.lineHeight
}

func (b *block) setText(size float64, lines []string) {
	b.size, b.lines, b.lineHeight = size, lines, size+leading
}

func gap(above, below block) float64 {
	if above.isQR || below.isQR {
		return qrGap
	}
	return textGap
}

func stackHeight(blocks []block) float64 {
	total := 0.0
	for i, b := range blocks {
		if i > 0 {
			total += gap(blocks[i-1], b)
		}
		total += b.height()
	}
	return total
}

// elementText is what an element prints for this ticket, "" for nothing.
// Badges are set in capitals.
func elementText(element string, t ticket.Ticket) string {
	text := t.Custom[element] // custom fields; unknown elements print nothing
	switch element {
	case "name":
		text = t.Name
	case "role":
		text = t.Type
		// Conference passes all read "Delegate" on the badge.
		if kind := strings.ToLower(t.Type); strings.Contains(kind, "conference") || strings.Contains(kind, "delegate") {
			text = "Delegate"
		}
	case "ticket_role":
		text = t.Role
	case "ticket_group":
		text = t.TicketGroup
	case "company":
		text = t.Company
	case "title":
		text = t.Title
	case "country":
		text = t.Country
	case "table_no":
		text = ""
		if t.TableNo != "" {
			text = "Table " + t.TableNo
		}
	}
	return strings.ToUpper(strings.TrimSpace(text))
}

// --- fitting text to a width ---

// wrap breaks text greedily into lines no wider than maxWidth. A single word
// wider than that gets a line of its own.
func wrap(text string, bold bool, size, maxWidth float64) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		candidate := strings.TrimSpace(line + " " + word)
		if stringWidth(candidate, bold, size) <= maxWidth {
			line = candidate
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
		line = word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// splitEvenly breaks text into the two lines closest in width that both fit,
// or nil if no split fits.
func splitEvenly(text string, bold bool, size, maxWidth float64) []string {
	words := strings.Fields(text)
	var best []string
	smallestDiff := math.Inf(1)
	for i := 1; i < len(words); i++ {
		first, second := strings.Join(words[:i], " "), strings.Join(words[i:], " ")
		w1, w2 := stringWidth(first, bold, size), stringWidth(second, bold, size)
		if diff := math.Abs(w1 - w2); w1 <= maxWidth && w2 <= maxWidth && diff < smallestDiff {
			smallestDiff, best = diff, []string{first, second}
		}
	}
	return best
}

// oneOrTwoLines fits text on one line, or failing that on two even ones.
func oneOrTwoLines(text string, bold bool, size, maxWidth float64) []string {
	if stringWidth(text, bold, size) <= maxWidth {
		return []string{text}
	}
	return splitEvenly(text, bold, size, maxWidth)
}

// fitHeadline picks the largest of sizes at which text fits on one or two
// lines: big type on two lines reads better than small type on one.
func fitHeadline(text string, bold bool, maxWidth float64, sizes []float64) (float64, []string) {
	for _, size := range sizes {
		if lines := oneOrTwoLines(text, bold, size, maxWidth); lines != nil {
			return size, lines
		}
	}
	const fallback = 11
	return fallback, wrap(text, bold, fallback, maxWidth)
}

// fitDetail sizes supporting text (company, title...): 13pt, stepping down as
// it needs more lines.
func fitDetail(text string, bold bool, maxWidth float64) (float64, []string) {
	lines := wrap(text, bold, 13, maxWidth)
	switch {
	case len(lines) > 3:
		return 9, wrap(text, bold, 9, maxWidth)
	case len(lines) > 2:
		return 11, wrap(text, bold, 11, maxWidth)
	}
	return 13, lines
}

// --- the three passes ---

// measure gives an element its natural size. ok is false when the element
// has nothing to print for this ticket.
func measure(element string, t ticket.Ticket, l config.Layout, maxWidth float64) (b block, ok bool) {
	scale := 1.0
	if s, set := l.ElementScales[element]; set {
		scale = s
	}
	if element == "qr" {
		return block{element: element, isQR: true, size: qrNaturalSize * scale, scale: scale, label: fitLabel(l.QRLabel, maxWidth)}, true
	}
	text := elementText(element, t)
	if text == "" {
		return block{}, false
	}
	bold := config.BoldByDefault(element)
	if override, set := l.ElementBolds[element]; set {
		bold = override
	}
	// A scaled element is fitted at natural size against a proportionally
	// narrower (or wider) box, so scaling changes the glyph size but not
	// where the lines break.
	fitWidth := maxWidth / scale
	var size float64
	var lines []string
	switch element {
	case "name":
		size, lines = fitHeadline(text, bold, fitWidth, nameSizes)
	case "role", "table_no":
		size, lines = fitHeadline(text, bold, fitWidth, headlineSizes)
	default:
		size, lines = fitDetail(text, bold, fitWidth)
	}
	b = block{element: element, bold: bold, scale: scale}
	b.setText(size*scale, lines)
	return b, true
}

// grow scales text up, a step at a time, while the stack is still short of
// its share of the paper. An element stops when it reaches its cap or its
// lines would get too wide.
func grow(blocks []block, t ticket.Ticket, paperHeight, maxWidth float64) []block {
	target := paperHeight * growFillTarget
	for stackHeight(blocks) < target {
		next := append([]block{}, blocks...)
		grewAny := false
		for i, b := range blocks {
			limit := maxGrownSize(b.element) * b.scale
			if b.isQR || b.size >= limit {
				continue
			}
			size := math.Min(b.size*(1+growStep), limit)
			lines := relayout(elementText(b.element, t), b.bold, size, maxWidth, len(b.lines))
			if lines == nil || widest(lines, b.bold, size) > maxWidth*growWidthLimit {
				continue
			}
			next[i].setText(size, lines)
			grewAny = true
		}
		if !grewAny || stackHeight(next) > target {
			break
		}
		blocks = next
	}
	return blocks
}

// relayout breaks text again at a larger size, or returns nil if it no
// longer fits. Text that was on one line is not split in two just to grow.
func relayout(text string, bold bool, size, maxWidth float64, lineCount int) []string {
	if stringWidth(text, bold, size) <= maxWidth {
		return []string{text}
	}
	if lineCount >= 2 {
		if lines := splitEvenly(text, bold, size, maxWidth); lines != nil {
			return lines
		}
	}
	if lines := wrap(text, bold, size, maxWidth); widest(lines, bold, size) <= maxWidth {
		return lines
	}
	return nil // a single word is wider than the badge at this size
}

func widest(lines []string, bold bool, size float64) float64 {
	width := 0.0
	for _, line := range lines {
		width = math.Max(width, stringWidth(line, bold, size))
	}
	return width
}

// shrink makes an over-tall stack fit: first the QR code down to its
// scannable minimum, then the largest text a point at a time. Line breaks
// are kept, so every step makes the stack shorter. If everything is at its
// minimum and it still doesn't fit, the badge prints clipped.
func shrink(blocks []block, available float64) {
	for stackHeight(blocks) > available {
		var qr, largest *block
		for i := range blocks {
			b := &blocks[i]
			switch {
			case b.isQR && b.size > qrMinSize:
				qr = b
			case !b.isQR && b.size > minTextSize && (largest == nil || b.size > largest.size):
				largest = b
			}
		}
		switch {
		case qr != nil:
			qr.size = math.Max(qrMinSize, qr.size-0.05*inch)
		case largest != nil:
			largest.setText(math.Max(minTextSize, largest.size-1), largest.lines)
		default:
			return
		}
	}
}

// compose lays the ticket out. It returns the blocks top to bottom, the
// height of the stack's top edge above the paper's bottom, and the paper
// size — all in points.
func compose(t ticket.Ticket, l config.Layout) (blocks []block, top, width, height float64) {
	width = l.Paper.WidthMM / 25.4 * inch
	height = l.Paper.HeightMM / 25.4 * inch
	maxWidth := width - 2*sideMargin

	for _, element := range l.Elements {
		if b, ok := measure(element, t, l, maxWidth); ok {
			blocks = append(blocks, b)
		}
	}
	blocks = grow(blocks, t, height, maxWidth)
	shrink(blocks, height-bottomMargin-edgeSafety)

	// Center the stack in the space above the bottom margin, then apply the
	// layout's vertical offset (positive = down) without letting the stack
	// leave the paper.
	stack := stackHeight(blocks)
	usable := height - bottomMargin
	top = height
	if stack < usable {
		top = bottomMargin + (usable+stack)/2
	}
	if l.VerticalOffsetMM != 0 {
		top -= l.VerticalOffsetMM / 25.4 * inch
		top = math.Max(bottomMargin+stack, math.Min(height, top))
	}
	return blocks, top, width, height
}

// fitLabel trims a QR caption with an ellipsis until it fits one line.
func fitLabel(label string, maxWidth float64) string {
	r := []rune(label)
	for len(r) > 0 && stringWidth(string(r), false, qrLabelSize) > maxWidth {
		r = r[:len(r)-1]
		label = string(r) + "…"
		if stringWidth(label, false, qrLabelSize) <= maxWidth {
			return label
		}
	}
	return string(r)
}
