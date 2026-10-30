package report

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

// PDF library choice: github.com/go-pdf/fpdf.
//
//   - Pure Go, zero cgo. It uses only the standard library plus golang.org/x
//     (already a dependency here), so `go build` keeps producing a static,
//     cross-compilable binary — no C toolchain, no per-arch surprises for the
//     multi-arch image builds.
//   - Maintained. It is the community-continued successor to the now-archived
//     jung-kurt/gofpdf, tagged releases as recently as this project's toolchain
//     window; the archived upstream is not.
//   - Licence-compatible. MIT, same as this repository.
//   - Self-contained rendering. The 14 PDF core fonts (Helvetica et al.) are
//     built into every reader, so no font files need embedding or shipping —
//     the report renders identically everywhere and the binary stays lean.

// Report palette. A print report is read on white paper, so the page stays
// white (dark backgrounds waste ink and read badly in print); the brand shows
// through a graphite masthead and the steel-blue accent that runs through the
// app's own design system. Uptime figures are traffic-lighted so the eye lands
// on the monitors that matter.
type rgb struct{ r, g, b int }

var (
	colGraphite = rgb{22, 24, 29}    // masthead band
	colAccent   = rgb{111, 143, 214} // #6f8fd6 — the app accent
	colAccentLt = rgb{150, 170, 222}
	colInk      = rgb{34, 37, 45}    // primary text
	colInkSoft  = rgb{74, 78, 88}    // numeric cells
	colMuted    = rgb{132, 137, 146} // labels, footer
	colGood     = rgb{40, 158, 100}  // >= 99.9%
	colWarn     = rgb{196, 146, 44}  // >= 95%
	colBad      = rgb{206, 78, 78}   // downtime / low uptime
	colZebra    = rgb{247, 248, 250}
	colHair     = rgb{228, 231, 236} // hairline rules & borders
	colCardFill = rgb{248, 249, 251}
)

// uptimeColor traffic-lights a fraction in 0..1: green at three-nines, amber
// through the 95%+ band, red below or whenever a monitor logged any downtime.
func uptimeColor(u float64) rgb {
	switch {
	case u >= 0.999:
		return colGood
	case u >= 0.95:
		return colWarn
	default:
		return colBad
	}
}

// WriteSLAPDF renders a print-ready SLA report to w: a branded masthead, a row
// of headline figures, a traffic-lighted per-monitor uptime table that repeats
// its header across page breaks, and a footer carrying the page number and the
// generation timestamp. The signature mirrors WriteSLACSV so the two exporters
// are interchangeable at the call site.
func WriteSLAPDF(w io.Writer, r Range, rows []SLARow) error {
	const (
		pageW    = 210.0
		pageH    = 297.0
		margin   = 14.0
		contentW = pageW - 2*margin
		bandH    = 25.0
		footerH  = 16.0
	)

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetTitle("Statwarden SLA Report", true)
	pdf.SetCreator("Statwarden", true)
	pdf.SetAuthor("Statwarden", true)
	pdf.SetMargins(margin, margin, margin)
	// Breaks are placed by hand (so the table header can be repeated), so the
	// automatic break is switched off to keep it from firing mid-row.
	pdf.SetAutoPageBreak(false, 0)

	// tr maps UTF-8 into the core fonts' cp1252 encoding so monitor names with
	// accents render (and anything outside cp1252 degrades to '?', never a
	// broken glyph run).
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	fill := func(c rgb) { pdf.SetFillColor(c.r, c.g, c.b) }
	text := func(c rgb) { pdf.SetTextColor(c.r, c.g, c.b) }
	draw := func(c rgb) { pdf.SetDrawColor(c.r, c.g, c.b) }

	// Footer: a hairline, the timestamp, and the page number, on every page.
	generated := time.Now().UTC().Format("2006-01-02 15:04 MST")
	pdf.SetFooterFunc(func() {
		pdf.SetY(-footerH + 4)
		draw(colHair)
		pdf.SetLineWidth(0.3)
		pdf.Line(margin, pdf.GetY(), pageW-margin, pdf.GetY())
		pdf.Ln(2)
		pdf.SetFont("Helvetica", "", 8)
		text(colMuted)
		pdf.CellFormat(contentW/2, 5, tr("Generated "+generated+"  ·  Statwarden"), "", 0, "L", false, 0, "")
		pdf.CellFormat(contentW/2, 5, fmt.Sprintf("Page %d", pdf.PageNo()), "", 0, "R", false, 0, "")
	})

	// Column geometry, shared by the header and every body row.
	type col struct {
		title string
		w     float64
		align string
	}
	cols := []col{
		{"Monitor", 64, "L"},
		{"Uptime", 26, "R"},
		{"Up", 22, "R"},
		{"Down", 22, "R"},
		{"Total", 26, "R"},
		{"p95 ms", 22, "R"},
	}
	drawTableHeader := func() {
		pdf.SetFont("Helvetica", "B", 9)
		fill(colAccent)
		text(rgb{255, 255, 255})
		for _, c := range cols {
			pdf.CellFormat(c.w, 8.5, tr(c.title), "", 0, c.align, true, 0, "")
		}
		pdf.Ln(-1)
	}

	pdf.AddPage()

	// --- Masthead -----------------------------------------------------------
	fill(colGraphite)
	pdf.Rect(0, 0, pageW, bandH, "F")
	fill(colAccent) // the mark: a small accent tile, echoing the app's logo
	pdf.RoundedRect(margin, 8.5, 8, 8, 1.6, "1234", "F")
	pdf.SetXY(margin+11.5, 6.8)
	pdf.SetFont("Helvetica", "B", 18)
	text(rgb{255, 255, 255})
	pdf.CellFormat(90, 9, "Statwarden", "", 0, "L", false, 0, "")
	pdf.SetXY(margin+11.5, 15.2)
	pdf.SetFont("Helvetica", "", 10)
	text(colAccentLt)
	pdf.CellFormat(90, 5, "SLA Report", "", 0, "L", false, 0, "")

	// Reporting period, right-aligned inside the band.
	pdf.SetFont("Helvetica", "", 9)
	pdf.SetXY(pageW-margin-92, 7.5)
	text(rgb{194, 198, 206})
	pdf.CellFormat(92, 4.6, tr(r.From.UTC().Format("2006-01-02 15:04 MST")), "", 2, "R", false, 0, "")
	pdf.SetX(pageW - margin - 92)
	text(colMuted)
	pdf.CellFormat(92, 4.4, "through", "", 2, "R", false, 0, "")
	pdf.SetX(pageW - margin - 92)
	text(rgb{194, 198, 206})
	pdf.CellFormat(92, 4.6, tr(r.To.UTC().Format("2006-01-02 15:04 MST")), "", 2, "R", false, 0, "")

	fill(colAccent) // accent rule closing the band
	pdf.Rect(0, bandH, pageW, 0.8, "F")

	// --- Aggregates ---------------------------------------------------------
	var totalUp, totalDown, totalChecks, monWithDown int
	var weightedUp, weight float64
	for _, row := range rows {
		totalUp += row.Uptime.Up
		totalDown += row.Uptime.Down
		totalChecks += row.Uptime.Total
		if row.Uptime.Down > 0 {
			monWithDown++
		}
		if row.Uptime.Uptime >= 0 && row.Uptime.Total > 0 {
			weightedUp += row.Uptime.Uptime * float64(row.Uptime.Total)
			weight += float64(row.Uptime.Total)
		}
	}
	overall := -1.0
	if weight > 0 {
		overall = weightedUp / weight
	}

	// --- Headline figures (KPI cards) --------------------------------------
	pdf.SetY(bandH + 8)
	cardY := pdf.GetY()
	const gap = 4.0
	cardW := (contentW - 3*gap) / 4
	const cardH = 21.0

	overallStr, overallColor := "n/a", colMuted
	if overall >= 0 {
		overallStr = strconv.FormatFloat(overall*100, 'f', 3, 64) + "%"
		overallColor = uptimeColor(overall)
	}
	downColor := colInk
	if monWithDown > 0 {
		downColor = colBad
	}
	kpis := []struct {
		label, val string
		color      rgb
	}{
		{"Overall uptime", overallStr, overallColor},
		{"Monitors", strconv.Itoa(len(rows)), colInk},
		{"With downtime", strconv.Itoa(monWithDown), downColor},
		{"Total checks", groupThousands(totalChecks), colInk},
	}
	x := margin
	for _, k := range kpis {
		fill(colCardFill)
		draw(colHair)
		pdf.SetLineWidth(0.3)
		pdf.RoundedRect(x, cardY, cardW, cardH, 2.2, "1234", "FD")
		// A thin accent tick at the top-left of each card.
		fill(colAccent)
		pdf.Rect(x, cardY, 1.4, cardH, "F")
		pdf.SetXY(x, cardY+4)
		pdf.SetFont("Helvetica", "B", 16)
		text(k.color)
		pdf.CellFormat(cardW, 8, k.val, "", 0, "C", false, 0, "")
		pdf.SetXY(x, cardY+13.5)
		pdf.SetFont("Helvetica", "", 8)
		text(colMuted)
		pdf.CellFormat(cardW, 4, tr(k.label), "", 0, "C", false, 0, "")
		x += cardW + gap
	}
	pdf.SetY(cardY + cardH + 9)

	// --- Per-monitor table --------------------------------------------------
	pdf.SetFont("Helvetica", "B", 11)
	text(colInk)
	pdf.CellFormat(0, 7, "Per-monitor uptime", "", 1, "L", false, 0, "")
	pdf.Ln(1.5)
	drawTableHeader()

	const rowH = 7.2
	bottomLimit := pageH - footerH
	for i, row := range rows {
		if pdf.GetY()+rowH > bottomLimit {
			pdf.AddPage()
			pdf.SetY(margin)
			drawTableHeader()
		}
		zebra := i%2 == 1
		if zebra {
			fill(colZebra)
		} else {
			fill(rgb{255, 255, 255})
		}
		draw(colHair)
		pdf.SetLineWidth(0.2)

		name := row.MonitorName
		if strings.TrimSpace(name) == "" {
			name = "#" + strconv.FormatInt(row.MonitorID, 10)
		}
		pdf.SetFont("Helvetica", "", 9)
		name = clipToWidth(pdf, tr(name), cols[0].w-4)

		uptimeStr, upColor := "n/a", colMuted
		if row.Uptime.Uptime >= 0 {
			uptimeStr = strconv.FormatFloat(row.Uptime.Uptime*100, 'f', 3, 64)
			upColor = uptimeColor(row.Uptime.Uptime)
		}
		p95 := "–"
		if row.Percentiles.P95 > 0 {
			p95 = strconv.Itoa(row.Percentiles.P95)
		}

		text(colInk)
		pdf.CellFormat(cols[0].w, rowH, name, "B", 0, "L", zebra, 0, "")
		pdf.SetFont("Helvetica", "B", 9)
		text(upColor)
		pdf.CellFormat(cols[1].w, rowH, uptimeStr, "B", 0, "R", zebra, 0, "")
		pdf.SetFont("Helvetica", "", 9)
		text(colInkSoft)
		pdf.CellFormat(cols[2].w, rowH, strconv.Itoa(row.Uptime.Up), "B", 0, "R", zebra, 0, "")
		if row.Uptime.Down > 0 {
			text(colBad)
		} else {
			text(colInkSoft)
		}
		pdf.CellFormat(cols[3].w, rowH, strconv.Itoa(row.Uptime.Down), "B", 0, "R", zebra, 0, "")
		text(colInkSoft)
		pdf.CellFormat(cols[4].w, rowH, strconv.Itoa(row.Uptime.Total), "B", 0, "R", zebra, 0, "")
		text(colMuted)
		pdf.CellFormat(cols[5].w, rowH, tr(p95), "B", 0, "R", zebra, 0, "")
		pdf.Ln(-1)
	}

	// --- Closing note -------------------------------------------------------
	pdf.Ln(6)
	if pdf.GetY()+16 < bottomLimit {
		pdf.SetFont("Helvetica", "", 9.5)
		text(colInkSoft)
		note := fmt.Sprintf(
			"%d monitors in scope; %s successful checks against %s failed (%d monitor(s) recorded downtime).",
			len(rows), groupThousands(totalUp), groupThousands(totalDown), monWithDown)
		pdf.MultiCell(0, 5.5, tr(note), "", "L", false)
	}

	if err := pdf.Output(w); err != nil {
		return fmt.Errorf("report: render SLA PDF: %w", err)
	}
	return nil
}

// clipToWidth trims s (already cp1252-encoded) to fit maxW mm in the current
// font, appending an ellipsis when it had to cut. It keeps long monitor names
// from spilling into the next column.
func clipToWidth(pdf *fpdf.Fpdf, s string, maxW float64) string {
	if pdf.GetStringWidth(s) <= maxW {
		return s
	}
	const ell = "…"
	for len(s) > 0 {
		s = s[:len(s)-1]
		if pdf.GetStringWidth(s+ell) <= maxW {
			return strings.TrimRight(s, " ") + ell
		}
	}
	return ell
}

// groupThousands renders n with thin thousands separators for the headline
// figures, so "128473" reads as "128,473".
func groupThousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
