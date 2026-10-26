//go:build windows

package gateway

// passwdShell has no Windows equivalent, and the CLI names powershell there, so
// hostShell falls back to the platform default.
func passwdShell() string { return "" }
