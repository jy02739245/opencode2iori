package gateway

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// The free tier fingerprints the agent it is talking to: the system prompt and
// the shell tool definition are part of that shape, and both name the host the
// official CLI runs on. These helpers report the same host facts the CLI would
// report, so a shaped request does not advertise a different machine than the
// client it claims to be.

// hostPlatform is the platform name the official CLI reports: Node's
// process.platform, which spells Windows "win32" where Go says "windows".
var hostPlatform = sync.OnceValue(func() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
})

// hostShell is the shell the official CLI names in its bash and shell tool
// descriptions. The CLI reads the running user's passwd entry (Node's
// os.userInfo().shell), not $SHELL: overriding SHELL in the environment left
// the capture unchanged, so a gateway that trusted the environment would drift
// from the client it claims to be. The platform's usual shell is the fallback.
var hostShell = sync.OnceValue(func() string {
	if shell := passwdShell(); shell != "" {
		return shell
	}
	if shell := filepath.Base(strings.TrimSpace(os.Getenv("SHELL"))); shell != "" && shell != "." && shell != string(filepath.Separator) {
		return shell
	}
	switch runtime.GOOS {
	case "darwin":
		return "zsh"
	case "windows":
		return "powershell"
	default:
		return "bash"
	}
})

// hostTempDir is the directory the official CLI offers for work outside the
// workspace: the process temp directory plus "opencode".
func hostTempDir() string {
	return filepath.Join(os.TempDir(), "opencode")
}

// hostPlatformTitle is the display name the 2.x client uses in its shell tool
// description ("Commands run on Linux using bash"). Only the Linux wording is
// captured, so the remaining names follow the CLI's platform naming; a 2.x
// capture from another host should replace them if it ever reads differently.
func hostPlatformTitle() string {
	name := hostPlatform()
	switch name {
	case "darwin":
		return "macOS"
	case "win32":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return strings.ToUpper(name[:1]) + name[1:]
	}
}
