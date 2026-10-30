//go:build !windows

package main

import "syscall"

// See errno_windows.go for why these are indirected through a variable rather
// than referenced directly at the use site.
var (
	errConnRefused error = syscall.ECONNREFUSED
	errConnReset   error = syscall.ECONNRESET
)
