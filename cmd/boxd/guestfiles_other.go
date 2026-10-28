//go:build !linux

package main

// prepareGuestFiles is a no-op off Linux, where boxd runs only in tests.
func prepareGuestFiles() {}
