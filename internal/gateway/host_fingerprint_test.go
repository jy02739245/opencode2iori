package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeRootMatchesV2Workspace(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	nested := filepath.Join(workspace, "src")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if root, inGit := worktreeRoot(nested); root != nested || inGit {
		t.Fatalf("nongit workspace = (%q, %v); want (%q, false)", root, inGit, nested)
	}
	for _, linked := range []bool{false, true} {
		git := filepath.Join(workspace, ".git")
		if linked {
			if err := os.WriteFile(git, []byte("gitdir: /external/worktrees/example"), 0600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Mkdir(git, 0700); err != nil {
			t.Fatal(err)
		}
		if root, inGit := worktreeRoot(nested); root != workspace || !inGit {
			t.Fatalf("git workspace (linked=%v) = (%q, %v); want (%q, true)", linked, root, inGit, workspace)
		}
		if err := os.Remove(git); err != nil {
			t.Fatal(err)
		}
	}
}

func TestV2ShellDescriptionMatchesHost(t *testing.T) {
	platform, shell := hostPlatform, hostShell
	t.Cleanup(func() { hostPlatform, hostShell = platform, shell })
	loadOfficialTools()
	definition := opencodeToolsV2["shell"]
	for _, tc := range []struct{ platform, shell, title string }{
		{"linux", "bash", "Linux"},
		{"darwin", "zsh", "macOS"},
		{"win32", "powershell", "Windows"},
	} {
		hostPlatform = func() string { return tc.platform }
		hostShell = func() string { return tc.shell }
		got := hostToolDescription("shell", definition.Description, false)
		want := "Commands run on " + tc.title + " using " + tc.shell + "."
		if !strings.Contains(got, want) {
			t.Fatalf("%s shell description lacks %q: %q", tc.platform, want, got)
		}
	}
}

func TestEnvironmentWorkspaceRootsByGeneration(t *testing.T) {
	opencodeEnvOnce.Do(resolveHostPaths)
	oldDirectory, oldRoot, oldInGit := opencodeWorkDir, opencodeRootDir, opencodeInGit
	t.Cleanup(func() { opencodeWorkDir, opencodeRootDir, opencodeInGit = oldDirectory, oldRoot, oldInGit })
	directory := t.TempDir()
	opencodeWorkDir, opencodeRootDir, opencodeInGit = directory, directory, false
	if got := opencodeEnvironmentV1("deepseek-v4.1-flash-free"); !strings.Contains(got, "Workspace root folder: "+workspaceRoot(directory)+"\n") {
		t.Fatalf("v1 nongit environment no longer reports the filesystem root: %q", got)
	}
	if got := opencodeEnvironmentV2(); !strings.Contains(got, "Workspace root folder: "+directory+"\n") {
		t.Fatalf("v2 nongit environment does not report the current workspace: %q", got)
	}
}
