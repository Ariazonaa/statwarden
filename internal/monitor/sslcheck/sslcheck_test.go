package sslcheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

func TestDaysRemaining(t *testing.T) {
	tests := []struct {
		name    string
		validTo time.Time
		want    int
	}{
		{"30 full days", testNow.Add(30 * 24 * time.Hour), 30},
		{"just under a day", testNow.Add(23 * time.Hour), 0},
		{"exactly now", testNow, 0},
		{"expired an hour ago", testNow.Add(-time.Hour), -1},
		{"expired two days ago", testNow.Add(-49 * time.Hour), -3},
		{"expired exactly one day ago", testNow.Add(-24 * time.Hour), -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DaysRemaining(tt.validTo, testNow); got != tt.want {
				t.Fatalf("DaysRemaining = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestActiveThreshold(t *testing.T) {
	tests := []struct {
		days          int
		wantThreshold int
		wantOK        bool
	}{
		{100, 0, false}, // comfortably valid
		{22, 0, false},  // just above the widest threshold
		{21, 21, true},
		{15, 21, true},
		{14, 14, true},
		{8, 14, true},
		{7, 7, true},
		{3, 3, true},
		{2, 3, true},
		{1, 1, true},
		{0, 1, true},
		{-1, 0, true}, // expired
	}
	for _, tt := range tests {
		threshold, ok := ActiveThreshold(tt.days, DefaultThresholds)
		if threshold != tt.wantThreshold || ok != tt.wantOK {
			t.Errorf("ActiveThreshold(%d) = (%d, %v), want (%d, %v)",
				tt.days, threshold, ok, tt.wantThreshold, tt.wantOK)
		}
	}
}

// selfSignedCert builds a throw-away certificate for inspection tests.
func selfSignedCert(t *testing.T, cn string, notBefore, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(4711),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"Statwarden Test"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestFromChain(t *testing.T) {
	notBefore := testNow.Add(-24 * time.Hour)
	notAfter := testNow.Add(14 * 24 * time.Hour)
	leaf := selfSignedCert(t, "monitor.example.com", notBefore, notAfter)

	info := FromChain([]*x509.Certificate{leaf}, testNow)
	if info == nil {
		t.Fatal("FromChain returned nil for non-empty chain")
	}
	if info.Serial != "4711" {
		t.Fatalf("Serial = %q, want 4711", info.Serial)
	}
	if info.DaysRemaining != 14 {
		t.Fatalf("DaysRemaining = %d, want 14", info.DaysRemaining)
	}
	if !info.ValidFrom.Equal(notBefore.Truncate(time.Second)) || !info.ValidTo.Equal(notAfter.Truncate(time.Second)) {
		t.Fatalf("validity = %v..%v, want %v..%v", info.ValidFrom, info.ValidTo, notBefore, notAfter)
	}
	if info.Subject == "" || info.Issuer == "" || len(info.FingerprintSHA256) != 64 {
		t.Fatalf("incomplete cert info: %+v", info)
	}

	if FromChain(nil, testNow) != nil {
		t.Fatal("FromChain(nil) must return nil")
	}
	if FromTLSState(nil, testNow) != nil {
		t.Fatal("FromTLSState(nil) must return nil")
	}
	cs := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	if got := FromTLSState(cs, testNow); got == nil || got.Serial != "4711" {
		t.Fatalf("FromTLSState = %+v, want leaf info", got)
	}
}

// memSource is an in-memory sweep source.
type memSource struct{ records []CertRecord }

func (s *memSource) ListCertInfo(context.Context) ([]CertRecord, error) {
	return s.records, nil
}

func TestSweeperThresholdEvents(t *testing.T) {
	src := &memSource{}
	var events []Event
	now := testNow
	sw := NewSweeper(SweeperOptions{
		Source:  src,
		OnEvent: func(ev Event) { events = append(events, ev) },
		Now:     func() time.Time { return now },
	})

	day := 24 * time.Hour
	record := func(validTo time.Time) {
		src.records = []CertRecord{{MonitorID: 1, MonitorName: "web", ValidTo: validTo}}
	}
	sweep := func() {
		t.Helper()
		if err := sw.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	// 60 days out: no event.
	record(now.Add(60 * day))
	sweep()
	if len(events) != 0 {
		t.Fatalf("events at 60 days = %v, want none", events)
	}

	// 20 days out: crosses the 21-day threshold once — repeated sweeps
	// stay silent.
	record(now.Add(20 * day))
	sweep()
	sweep()
	if len(events) != 1 || events[0].Threshold != 21 || events[0].Expired {
		t.Fatalf("events at 20 days = %+v, want one 21-day event", events)
	}

	// Time passes to 13 days: next threshold (14) fires.
	now = now.Add(7 * day)
	sweep()
	if len(events) != 2 || events[1].Threshold != 14 {
		t.Fatalf("events at 13 days = %+v, want added 14-day event", events)
	}

	// Certificate expires: threshold 0, Expired=true.
	now = now.Add(20 * day)
	sweep()
	if len(events) != 3 || !events[2].Expired || events[2].DaysRemaining >= 0 {
		t.Fatalf("events after expiry = %+v, want expired event", events)
	}

	// Renewal far into the future resets tracking, so a later approach
	// alerts again.
	record(now.Add(90 * day))
	sweep()
	record(now.Add(10 * day))
	sweep()
	if len(events) != 4 || events[3].Threshold != 14 {
		t.Fatalf("events after renewal = %+v, want fresh 14-day event", events)
	}
}
