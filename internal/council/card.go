package council

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Cards are sized for posting on X.
const (
	cardW      = 1600
	cardH      = 900
	cardMargin = 88
)

// Card colors: a dark background with high-contrast text.
var (
	colBgTop    = color.RGBA{0x0b, 0x0f, 0x17, 0xff}
	colBgBottom = color.RGBA{0x13, 0x1a, 0x27, 0xff}
	colText     = color.RGBA{0xf5, 0xf7, 0xfa, 0xff}
	colMuted    = color.RGBA{0x9a, 0xa6, 0xb8, 0xff}
	colAccent   = color.RGBA{0x5e, 0xea, 0xd4, 0xff}
	colGold     = color.RGBA{0xfb, 0xbf, 0x24, 0xff}
	colWarn     = color.RGBA{0xf8, 0x71, 0x71, 0xff}
	colTrack    = color.RGBA{0x23, 0x2c, 0x3b, 0xff}
	colLine     = color.RGBA{0x2a, 0x34, 0x46, 0xff}
)

// The cards' embedded fonts, parsed once.
var cardFonts = sync.OnceValues(func() (map[string]*sfnt.Font, error) {
	out := map[string]*sfnt.Font{}
	for name, ttf := range map[string][]byte{"regular": goregular.TTF, "medium": gomedium.TTF, "bold": gobold.TTF} {
		f, err := opentype.Parse(ttf)
		if err != nil {
			return nil, fmt.Errorf("card font %s: %w", name, err)
		}
		out[name] = f
	}
	return out, nil
})

// hasGlyph is whether the cards' font draws r.
func hasGlyph(f *sfnt.Font, buf *sfnt.Buffer, r rune) bool {
	i, err := f.GlyphIndex(buf, r)
	return err == nil && i != 0
}

// replacementRune stands in for a character the font cannot draw: U+FFFD
// when the font has it, else "?".
var replacementRune = sync.OnceValue(func() rune {
	fonts, err := cardFonts()
	if err == nil && hasGlyph(fonts["regular"], &sfnt.Buffer{}, '�') {
		return '�'
	}
	return '?'
})

// cardText makes s drawable on one line: whitespace becomes single
// spaces, invisible joiners and emoji modifiers go, and each run of
// characters the font lacks becomes one replacement glyph, so nothing is
// dropped silently.
func cardText(s string) string {
	fonts, err := cardFonts()
	if err != nil {
		return s
	}
	f := fonts["regular"]
	var buf sfnt.Buffer
	repl := replacementRune()
	var b strings.Builder
	lastRepl, lastSpace := false, true
	for _, r := range s {
		switch {
		case unicode.IsSpace(r) || unicode.IsControl(r):
			if !lastSpace {
				b.WriteRune(' ')
			}
			lastSpace, lastRepl = true, false
			continue
		case r == '‍' || r == '​' || r == '︎' || r == '️' || (r >= 0x1f3fb && r <= 0x1f3ff) || (r >= 0xe0020 && r <= 0xe007f):
			continue // joins or modifies a neighbor; nothing to draw
		case !hasGlyph(f, &buf, r):
			if !lastRepl {
				b.WriteRune(repl)
			}
			lastRepl, lastSpace = true, false
			continue
		}
		b.WriteRune(r)
		lastRepl, lastSpace = false, false
	}
	return strings.TrimSpace(b.String())
}

// canvas is a card being drawn.
type canvas struct {
	img   *image.RGBA
	fonts map[string]*sfnt.Font
	faces map[string]font.Face
}

func newCanvas() (*canvas, error) {
	fonts, err := cardFonts()
	if err != nil {
		return nil, err
	}
	c := &canvas{img: image.NewRGBA(image.Rect(0, 0, cardW, cardH)), fonts: fonts, faces: map[string]font.Face{}}
	for y := range cardH {
		t := float64(y) / float64(cardH-1)
		row := color.RGBA{mix(colBgTop.R, colBgBottom.R, t), mix(colBgTop.G, colBgBottom.G, t), mix(colBgTop.B, colBgBottom.B, t), 0xff}
		draw.Draw(c.img, image.Rect(0, y, cardW, y+1), image.NewUniform(row), image.Point{}, draw.Src)
	}
	draw.Draw(c.img, image.Rect(0, 0, cardW, 6), image.NewUniform(colAccent), image.Point{}, draw.Src)
	return c, nil
}

func mix(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t + 0.5) }

// face is the named font at size pixels.
func (c *canvas) face(name string, size float64) font.Face {
	key := fmt.Sprintf("%s/%g", name, size)
	if f, ok := c.faces[key]; ok {
		return f
	}
	f, err := opentype.NewFace(c.fonts[name], &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		f = basicfont.Face7x13 // only bad options fail, and these are fixed
	}
	c.faces[key] = f
	return f
}

func (c *canvas) close() {
	for _, f := range c.faces {
		f.Close()
	}
}

// text draws s with its baseline at y, starting at x.
func (c *canvas) text(f font.Face, x, y int, col color.Color, s string) {
	d := font.Drawer{Dst: c.img, Src: image.NewUniform(col), Face: f, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

// textRight draws s with its baseline at y, ending at x.
func (c *canvas) textRight(f font.Face, x, y int, col color.Color, s string) {
	c.text(f, x-width(f, s), y, col, s)
}

// width is how wide s draws in f, in pixels.
func width(f font.Face, s string) int { return font.MeasureString(f, s).Ceil() }

// roundRect fills an anti-aliased rectangle with corner radius r.
func (c *canvas) roundRect(x0, y0, x1, y1, r float32, col color.Color) {
	if x1-x0 < 1 || y1-y0 < 1 {
		return
	}
	r = min(r, (x1-x0)/2, (y1-y0)/2)
	ox, oy := int(x0), int(y0)
	w, h := int(x1)-ox+1, int(y1)-oy+1
	x0, x1, y0, y1 = x0-float32(ox), x1-float32(ox), y0-float32(oy), y1-float32(oy)
	z := vector.NewRasterizer(w, h)
	z.MoveTo(x0+r, y0)
	z.LineTo(x1-r, y0)
	z.QuadTo(x1, y0, x1, y0+r)
	z.LineTo(x1, y1-r)
	z.QuadTo(x1, y1, x1-r, y1)
	z.LineTo(x0+r, y1)
	z.QuadTo(x0, y1, x0, y1-r)
	z.LineTo(x0, y0+r)
	z.QuadTo(x0, y0, x0+r, y0)
	z.ClosePath()
	z.Draw(c.img, image.Rect(ox, oy, ox+w, oy+h), image.NewUniform(col), image.Point{})
}

// scoreBar draws a bar from x0 to x1 centered on y, filled to score (0 to 1).
func (c *canvas) scoreBar(x0, x1, y, h int, score float64, fill color.Color) {
	fx, fy, fh := float32(x0), float32(y)-float32(h)/2, float32(h)
	c.roundRect(fx, fy, float32(x1), fy+fh, fh/2, colTrack)
	score = max(0, min(1, score))
	c.roundRect(fx, fy, fx+max(fh, float32(x1-x0)*float32(score)), fy+fh, fh/2, fill)
}

// ellipsize cuts s to fit maxW, ending it with an ellipsis when it cut.
func ellipsize(f font.Face, s string, maxW int) string {
	if width(f, s) <= maxW {
		return s
	}
	return withEllipsis(f, s, maxW)
}

// withEllipsis ends s with an ellipsis, cutting s until both fit maxW.
func withEllipsis(f font.Face, s string, maxW int) string {
	rs := []rune(s)
	for len(rs) > 0 {
		cut := strings.TrimRight(string(rs), " .,;:-")
		if width(f, cut+"…") <= maxW {
			return cut + "…"
		}
		rs = rs[:len(rs)-1]
	}
	return "…"
}

// wrap breaks s into at most maxLines lines of maxW, the last one
// ellipsized when s does not fit. A word wider than a line is broken.
func wrap(f font.Face, s string, maxW, maxLines int) []string {
	var lines []string
	cur := ""
	for word := range strings.FieldsSeq(s) {
		for width(f, word) > maxW {
			// Break an over-long word at the most runes that fit.
			rs := []rune(word)
			n := len(rs) - 1
			for n > 1 && width(f, string(rs[:n])) > maxW {
				n--
			}
			if cur != "" {
				lines = append(lines, cur)
				cur = ""
			}
			lines = append(lines, string(rs[:n]))
			word = string(rs[n:])
		}
		switch {
		case cur == "":
			cur = word
		case width(f, cur+" "+word) <= maxW:
			cur += " " + word
		default:
			lines = append(lines, cur)
			cur = word
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] = withEllipsis(f, lines[maxLines-1], maxW)
	}
	return lines
}

// header draws the brand line with right-aligned detail.
func (c *canvas) header(right string) {
	f := c.face("medium", 24)
	c.text(f, cardMargin, 84, colAccent, "AGENT TINCAN COUNCIL")
	c.textRight(f, cardW-cardMargin, 84, colMuted, cardText(right))
}

// footer draws the branding and repo link with a right-aligned note.
func (c *canvas) footer(right string) {
	draw.Draw(c.img, image.Rect(cardMargin, 812, cardW-cardMargin, 813), image.NewUniform(colLine), image.Point{}, draw.Src)
	bold := c.face("bold", 28)
	c.text(bold, cardMargin, 862, colText, "Agent Tincan")
	c.text(c.face("regular", 24), cardMargin+width(bold, "Agent Tincan")+20, 861, colAccent, RepoURL)
	c.textRight(c.face("regular", 22), cardW-cardMargin, 861, colMuted, cardText(right))
}

func (c *canvas) encode(w io.Writer) error {
	c.close()
	return png.Encode(w, c.img)
}

// markdownMarks are markdown characters dropped from card text.
var markdownMarks = strings.NewReplacer("**", "", "__", "", "`", "", "*", "")

// verdictLine is the scorecard's one-line verdict: the chairman's
// recommendation with answers named by member, or why there is none.
func verdictLine(f Finished) string {
	o := f.Outcome
	switch {
	case o.State == CouncilDeclined:
		return "Council declined: " + o.Reason
	case o.State == CouncilFailed:
		return "No verdict: " + o.Reason
	case o.Verdict.Unavailable:
		return "Verdict unavailable. Ranked by blind peer review alone."
	}
	// Each "Answer K" is replaced by the member's name.
	rec := replaceLabels(o.Verdict.Recommendation, o.Verdict.Labels, func(_, member string) string { return member })
	rec = markdownMarks.Replace(rec)
	return strings.TrimLeft(strings.Join(strings.Fields(rec), " "), "#>- ")
}

// DrawScorecard writes f's scorecard as a 1600x900 PNG: the question, the
// members in peer-ranked order with score bars, the verdict in one line,
// and the Agent Tincan branding with the repo link.
func DrawScorecard(w io.Writer, f Finished) error {
	c, err := newCanvas()
	if err != nil {
		return err
	}
	o := f.Outcome
	right := ""
	if o.State == CouncilCompleted {
		right = strings.ToUpper(f.category()) + "  /  "
	}
	if !f.At.IsZero() {
		right += f.At.Format("Jan 2, 2006")
	}
	c.header(strings.TrimSuffix(right, "  /  "))

	inner := cardW - 2*cardMargin
	qFace := c.face("bold", 50)
	q := wrap(qFace, cardText(f.Question), inner, 3)
	y := 168
	for _, l := range q {
		c.text(qFace, cardMargin, y, colText, l)
		y += 62
	}

	top, bottom := y+14, 690
	if len(o.Standings) > 0 {
		c.text(c.face("medium", 20), cardMargin, top+6, colMuted, "BLIND PEER RANKING")
		c.textRight(c.face("medium", 20), cardW-cardMargin, top+6, colMuted, "SCORE")
		rows := o.Standings
		more := 0
		maxRows := 10
		if len(rows) > maxRows {
			more = len(rows) - (maxRows - 1)
			rows = rows[:maxRows-1]
		}
		n := len(rows)
		if more > 0 {
			n++
		}
		rowH := min(68, (bottom-top-24)/n)
		size := float64(max(20, min(34, rowH*52/100)))
		for i, s := range rows {
			cy := top + 24 + rowH*i + rowH/2
			base := cy + int(size*0.36)
			first := s.Placement == 1
			numCol, nameCol, barCol, nameFace := colMuted, colText, colAccent, c.face("medium", size)
			if first {
				numCol, barCol, nameFace = colGold, colGold, c.face("bold", size)
			}
			c.text(c.face("bold", size), cardMargin, base, numCol, fmt.Sprintf("%d", s.Placement))
			c.text(nameFace, cardMargin+64, base, nameCol, ellipsize(nameFace, cardText(s.Member), 470))
			c.scoreBar(cardMargin+560, cardW-cardMargin-130, cy, max(10, rowH*30/100), s.Score, barCol)
			c.textRight(c.face("bold", size), cardW-cardMargin, base, nameCol, fmt.Sprintf("%.2f", s.Score))
		}
		if more > 0 {
			c.text(c.face("regular", size*0.8), cardMargin+64, top+24+rowH*len(rows)+rowH/2+int(size*0.3), colMuted, fmt.Sprintf("+%d more in the report", more))
		}
	} else {
		title := "No quorum"
		if o.State == CouncilDeclined {
			title = "Council declined"
		}
		c.text(c.face("bold", 44), cardMargin, top+70, colWarn, title)
		if len(o.Answers) > 0 {
			var names []string
			for _, a := range o.Answers {
				names = append(names, a.Member)
			}
			rf := c.face("regular", 30)
			c.text(rf, cardMargin, top+130, colMuted, ellipsize(rf, cardText("Answered: "+strings.Join(names, ", ")), inner))
		}
	}

	c.text(c.face("medium", 20), cardMargin, 736, colAccent, verdictLabel(f))
	vf := c.face("regular", 32)
	vcol := colText
	if !f.verdictShown() {
		vcol = colMuted
	}
	c.text(vf, cardMargin, 780, vcol, ellipsize(vf, cardText(verdictLine(f)), inner))

	c.footer(f.statusLine())
	return c.encode(w)
}

// verdictLabel heads the scorecard's verdict line.
func verdictLabel(f Finished) string {
	if f.verdictShown() {
		return strings.ToUpper("Verdict by " + cardText(f.Outcome.Verdict.Chairman))
	}
	return "VERDICT"
}

// DrawLeaderboard writes the leaderboard for category ("" is overall) as
// a 1600x900 PNG: members ranked by wins, then mean peer score.
func DrawLeaderboard(w io.Writer, rows []LeaderboardRow, category string, at time.Time) error {
	c, err := newCanvas()
	if err != nil {
		return err
	}
	c.header(at.Format("Jan 2, 2006"))
	scope := "Overall"
	if category != "" {
		scope = strings.ToUpper(category[:1]) + strings.ReplaceAll(category[1:], "-", " ")
	}
	tf := c.face("bold", 60)
	c.text(tf, cardMargin, 178, colText, ellipsize(tf, cardText("Leaderboard: "+scope), cardW-2*cardMargin))
	c.text(c.face("regular", 26), cardMargin, 224, colMuted, "Ranked by council wins, then mean score from blind peer review")

	top, bottom := 262, 790
	colWins, colCouncils := cardMargin+700, cardMargin+880
	barX0, barX1 := cardMargin+940, cardW-cardMargin-110
	hf := c.face("medium", 18)
	c.text(hf, cardMargin, top+10, colMuted, "#")
	c.text(hf, cardMargin+64, top+10, colMuted, "MEMBER")
	c.textRight(hf, colWins, top+10, colMuted, "WINS")
	c.textRight(hf, colCouncils, top+10, colMuted, "COUNCILS")
	c.text(hf, barX0, top+10, colMuted, "MEAN PEER SCORE")

	if len(rows) == 0 {
		c.text(c.face("medium", 36), cardMargin, top+120, colMuted, "No scored councils yet.")
	}
	more := 0
	maxRows := 10
	if len(rows) > maxRows {
		more = len(rows) - (maxRows - 1)
		rows = rows[:maxRows-1]
	}
	n := max(len(rows), 1)
	if more > 0 {
		n++
	}
	rowH := min(72, (bottom-top-28)/n)
	size := float64(max(20, min(36, rowH*52/100)))
	for i, r := range rows {
		cy := top + 28 + rowH*i + rowH/2
		base := cy + int(size*0.36)
		numCol, barCol, nameFace := colMuted, colAccent, c.face("medium", size)
		if i == 0 {
			numCol, barCol, nameFace = colGold, colGold, c.face("bold", size)
		}
		bf := c.face("bold", size)
		c.text(bf, cardMargin, base, numCol, fmt.Sprintf("%d", i+1))
		c.text(nameFace, cardMargin+64, base, colText, ellipsize(nameFace, cardText(r.Member), 520))
		c.textRight(bf, colWins, base, colText, fmt.Sprintf("%d", r.Wins))
		c.textRight(c.face("regular", size), colCouncils, base, colMuted, fmt.Sprintf("%d", r.Councils))
		c.scoreBar(barX0, barX1, cy, max(10, rowH*28/100), r.MeanScore, barCol)
		c.textRight(bf, cardW-cardMargin, base, colText, fmt.Sprintf("%.2f", r.MeanScore))
	}
	if more > 0 {
		c.text(c.face("regular", size*0.8), cardMargin+64, top+28+rowH*len(rows)+rowH/2+int(size*0.3), colMuted, fmt.Sprintf("+%d more", more))
	}

	c.footer("Cross-model council, blind peer review")
	return c.encode(w)
}
