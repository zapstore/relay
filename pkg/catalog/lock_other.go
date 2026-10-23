//go:build !unix

package catalog

// lockSeal is a no-op where flock is missing. The relay runs on Linux and macOS.
func lockSeal(string) (func(), error) { return func() {}, nil }
