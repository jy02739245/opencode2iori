//go:build !windows

package gateway

import (
	"os"
	"strconv"
	"strings"
)

// passwdShell returns the login shell of the running user from /etc/passwd, the
// entry Node's os.userInfo().shell reports. It returns "" when the entry has no
// shell, so the caller can fall back to the platform default.
func passwdShell() string {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	uid := strconv.Itoa(os.Getuid())
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 || fields[2] != uid {
			continue
		}
		shell := strings.TrimSpace(fields[6])
		if idx := strings.LastIndex(shell, "/"); idx >= 0 {
			shell = shell[idx+1:]
		}
		return shell
	}
	return ""
}
