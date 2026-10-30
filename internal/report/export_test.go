package report

import (
	"bytes"
	"encoding/csv"
	"testing"
	"time"
)

// A monitor name is operator-supplied text that lands in a file someone opens
// in Excel, where a leading =, +, - or @ makes the cell a formula rather than
// text. CSV quoting does not prevent it: the quotes are consumed by the parser
// and the formula character is the first thing left in the cell.
func TestCSVNeutralisesSpreadsheetFormulas(t *testing.T) {
	dangerous := []string{
		`=HYPERLINK("https://attacker.test","click")`,
		`+1+1`,
		`-1+1`,
		`@SUM(A1:A9)`,
		"\tleading tab",
		"\rleading cr",
	}
	for _, name := range dangerous {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			err := WriteSLACSV(&buf, Range{From: time.Unix(0, 0), To: time.Unix(1, 0)},
				[]SLARow{{MonitorID: 1, MonitorName: name}})
			if err != nil {
				t.Fatalf("WriteSLACSV: %v", err)
			}
			recs, err := csv.NewReader(&buf).ReadAll()
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if len(recs) != 2 {
				t.Fatalf("got %d records, want header + one row", len(recs))
			}
			cell := recs[1][1]
			if cell[0] != '\'' {
				t.Fatalf("cell %q is not neutralised; a spreadsheet evaluates it", cell)
			}
			// The name must still be readable — neutralising is not censoring.
			if cell[1:] != name {
				t.Fatalf("cell = %q, want an apostrophe followed by %q", cell, name)
			}
		})
	}
}

// An ordinary name passes through untouched: a report full of stray
// apostrophes would be its own kind of broken.
func TestCSVLeavesOrdinaryNamesAlone(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteSLACSV(&buf, Range{From: time.Unix(0, 0), To: time.Unix(1, 0)},
		[]SLARow{{MonitorID: 1, MonitorName: "api.example.com"}}); err != nil {
		t.Fatalf("WriteSLACSV: %v", err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if recs[1][1] != "api.example.com" {
		t.Fatalf("ordinary name was altered: %q", recs[1][1])
	}
}
