//go:build windows

package main

import "syscall"

// Windows reports socket failures with Winsock error codes, not the POSIX
// errno values that syscall.ECONNREFUSED and syscall.ECONNRESET carry there.
// Those two are synthetic constants (ECONNREFUSED is 0x2000_0016) that never
// appear in a real *net.OpError, so an errors.Is against them silently never
// fires and every refused connection would be misreported as a generic failure.
//
// The stdlib's syscall package does not export the Winsock names on Windows —
// only golang.org/x/sys/windows does, and that is not worth a direct dependency
// for two integers — so they are spelled out here.
const (
	wsaeConnReset   = syscall.Errno(10054) // WSAECONNRESET
	wsaeConnRefused = syscall.Errno(10061) // WSAECONNREFUSED
)

var (
	errConnRefused error = wsaeConnRefused
	errConnReset   error = wsaeConnReset
)
