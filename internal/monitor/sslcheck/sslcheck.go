// Package sslcheck provides shared TLS certificate inspection for all
// TLS-capable monitor types plus the daily certificate-expiry sweep that
// raises threshold alerts independently of check intervals.
package sslcheck

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// FromTLSState extracts the leaf certificate info from a completed TLS
// handshake (e.g. http.Response.TLS or tls.Conn.ConnectionState()). It
// returns nil when no peer certificate is present.
func FromTLSState(cs *tls.ConnectionState, now time.Time) *domain.CertInfo {
	if cs == nil {
		return nil
	}
	return FromChain(cs.PeerCertificates, now)
}

// FromChain extracts the leaf (first) certificate of a presented chain.
// It returns nil for an empty chain.
func FromChain(chain []*x509.Certificate, now time.Time) *domain.CertInfo {
	if len(chain) == 0 {
		return nil
	}
	leaf := chain[0]
	sum := sha256.Sum256(leaf.Raw)
	return &domain.CertInfo{
		Subject:           leaf.Subject.String(),
		Issuer:            leaf.Issuer.String(),
		Serial:            leaf.SerialNumber.String(),
		FingerprintSHA256: hex.EncodeToString(sum[:]),
		ValidFrom:         leaf.NotBefore,
		ValidTo:           leaf.NotAfter,
		DaysRemaining:     DaysRemaining(leaf.NotAfter, now),
	}
}

// DaysRemaining returns the whole days until validTo, rounded down; expired
// certificates yield negative values.
func DaysRemaining(validTo, now time.Time) int {
	remaining := validTo.Sub(now)
	days := int(remaining / (24 * time.Hour))
	if remaining < 0 && remaining%(24*time.Hour) != 0 {
		days-- // floor, not truncate, for expired certs
	}
	return days
}
