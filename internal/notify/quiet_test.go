package notify

import (
	"testing"
	"time"
)

func TestQuietHoursValidate(t *testing.T) {
	tests := []struct {
		name    string
		q       QuietHours
		wantErr bool
	}{
		{"valid daytime", QuietHours{Start: "09:00", End: "17:00"}, false},
		{"valid overnight", QuietHours{Start: "22:00", End: "07:00"}, false},
		{"empty window", QuietHours{Start: "08:00", End: "08:00"}, true},
		{"bad start", QuietHours{Start: "25:00", End: "07:00"}, true},
		{"bad end", QuietHours{Start: "22:00", End: "7"}, true},
		{"not a time", QuietHours{Start: "abc", End: "07:00"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.q.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestQuietHoursContains(t *testing.T) {
	at := func(h, m int) time.Time {
		return time.Date(2026, 7, 20, h, m, 0, 0, time.UTC)
	}
	tests := []struct {
		name string
		q    QuietHours
		when time.Time
		want bool
	}{
		{"daytime inside", QuietHours{Start: "09:00", End: "17:00"}, at(12, 0), true},
		{"daytime before", QuietHours{Start: "09:00", End: "17:00"}, at(8, 59), false},
		{"daytime at start", QuietHours{Start: "09:00", End: "17:00"}, at(9, 0), true},
		{"daytime at end exclusive", QuietHours{Start: "09:00", End: "17:00"}, at(17, 0), false},
		{"overnight late", QuietHours{Start: "22:00", End: "07:00"}, at(23, 30), true},
		{"overnight early", QuietHours{Start: "22:00", End: "07:00"}, at(6, 0), true},
		{"overnight midday out", QuietHours{Start: "22:00", End: "07:00"}, at(12, 0), false},
		{"overnight at end exclusive", QuietHours{Start: "22:00", End: "07:00"}, at(7, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.q.Contains(tt.when); got != tt.want {
				t.Fatalf("Contains(%v) = %v, want %v", tt.when, got, tt.want)
			}
		})
	}
}

func TestQuietHoursTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tz database unavailable: %v", err)
	}
	q := QuietHours{Start: "22:00", End: "07:00", Location: loc}
	// 21:30 UTC in July is 23:30 Berlin (CEST, +2) — inside the window.
	utc := time.Date(2026, 7, 20, 21, 30, 0, 0, time.UTC)
	if !q.Contains(utc) {
		t.Fatal("expected 23:30 Berlin to be inside quiet hours")
	}
	// 09:00 UTC is 11:00 Berlin — outside.
	if q.Contains(time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)) {
		t.Fatal("expected 11:00 Berlin to be outside quiet hours")
	}
}
