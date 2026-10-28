package gateway

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	wire "opencode2api/internal/protocol"
)

// The free tier accepts only agent-shaped requests, and its abuse checks expect
// the system prompt the official CLI sends. These assets are the CLI's own
// wording: the v1 text for /zen (OpenCode 1.x tool names) and the v2 text for
// /inference and /zen/go (OpenCode 2.x tool names). The v2 file is the v1 file
// with the tool names the 2.x CLI uses.
//
//go:embed opencode_prompt_v1.txt
var opencodePromptV1 string

//go:embed opencode_prompt_v2.txt
var opencodePromptV2 string

//go:embed opencode_skills_v2.txt
var opencodeSkillsV2 string

// opencodePromptMarker identifies a body that already carries the official CLI
// system prompt, so a client that sends it itself is left alone. The two
// generations open differently: 1.x with "You are opencode, an interactive CLI
// tool", 2.x with "You are an AI agent running in OpenCode".
const (
	opencodePromptMarker   = "You are opencode, an interactive CLI tool"
	opencodePromptMarkerV2 = "You are an AI agent running in OpenCode"
)

// opencodeSkillsV1 is the 1.x built-in skill listing the CLI appends after its
// environment block. It is static, so it is part of the injected prompt. The
// 2.x client lists different skills and puts the block before the environment,
// so its listing lives in opencode_skills_v2.txt.
const opencodeSkillsV1 = `Skills provide specialized instructions and workflows for specific tasks.
Use the skill tool to load a skill when a task matches its description.
<available_skills>
  <skill>
    <name>customize-opencode</name>
    <description>Use ONLY when the user is editing or creating opencode's own configuration: opencode.json, opencode.jsonc, files under .opencode/, or files under ~/.config/opencode/. Also use when creating or fixing opencode agents, subagents, skills, plugins, MCP servers, or permission rules. Do not use for the user's own application code, or for any project that is not configuring opencode itself.</description>
    <location>&lt;built-in&gt;</location>
  </skill>
</available_skills>`

var (
	opencodeEnvOnce sync.Once
	opencodeWorkDir string
	opencodeRootDir string
	opencodeInGit   bool
)

// opencodeEnvironment renders the <env> block the CLI puts after its base
// prompt. The directory is resolved once per process; a platform without one
// falls back to the official container path. The CLI reports the directory it
// runs in, the root of the git worktree that contains it (or the filesystem
// root when there is none) and whether that worktree exists, so the block never
// contradicts the project header the gateway sends alongside it.
func opencodeEnvironmentV1(model string) string {
	opencodeEnvOnce.Do(resolveHostPaths)
	apiModel := strings.TrimSpace(strings.TrimPrefix(model, "opencode/"))
	if apiModel == "" {
		apiModel = "unknown"
	}
	return strings.Join([]string{
		"You are powered by the model named " + apiModel + ". The exact model ID is opencode/" + apiModel,
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + opencodeWorkDir,
		"  Workspace root folder: " + opencodeRootDir,
		"  Is directory a git repo: " + repoFlag(),
		"  Platform: " + hostPlatform(),
		"  Today's date: " + today(),
		"</env>",
	}, "\n")
}

// opencodeEnvironmentV2 renders the 2.2 client's environment tail. It differs
// from 1.x in three ways: the date stands on its own before the block, the
// block names the temp directory the client pre-created instead of the date,
// and a note about moving the session to a worktree follows it. The 2.x client
// also names no model, because its skills listing already precedes this text.
func opencodeEnvironmentV2() string {
	opencodeEnvOnce.Do(resolveHostPaths)
	return "\nToday's date: " + today() +
		"\n\nHere is some useful information about the environment you are running in:\n<env>\n" +
		"  Working directory: " + opencodeWorkDir + "\n" +
		"  Workspace root folder: " + opencodeRootDir + "\n" +
		"  Is directory a git repo: " + repoFlag() + "\n" +
		"  Platform: " + hostPlatform() + "\n" +
		"  Prefer " + hostTempDir() + " over generic system temporary directories such as /tmp; it is pre-created and approved for external access.\n" +
		"</env>\n" +
		"When you create a worktree outside the current working directory and intend to use it as your primary working directory, consider using `execute` to call `tools.opencode.session_move` and make the worktree the session's working directory."
}

// resolveHostPaths resolves the working directory, its git worktree root and
// the repository flag once per process.
func resolveHostPaths() {
	directory := ""
	if current, err := os.Getwd(); err == nil {
		directory = filepath.Clean(strings.TrimSpace(current))
	}
	if directory == "" || directory == "." {
		directory = "/workspace"
	}
	// The CLI reports the real path, so a working directory reached through a
	// symlink must not keep the link (macOS resolves /tmp to /private/tmp).
	if resolved, err := filepath.EvalSymlinks(directory); err == nil && resolved != "" {
		directory = resolved
	}
	opencodeWorkDir = directory
	opencodeRootDir, opencodeInGit = worktreeRoot(directory)
}

// repoFlag is the value of the environment block's repository line.
func repoFlag() string {
	if opencodeInGit {
		return "yes"
	}
	return "no"
}

// today is the date both generations print. The CLI uses the host's local date,
// not UTC: a capture under TZ=Pacific/Kiritimati reported the next day while UTC
// still named the previous one.
func today() string {
	return time.Now().Format("Mon Jan 02 2006")
}

// worktreeRoot reports the root of the git worktree containing directory, or
// the filesystem root when directory is not inside one. A worktree root is the
// nearest ancestor carrying a .git entry, which is a directory in a ordinary
// checkout and a file in a linked worktree.
func worktreeRoot(directory string) (string, bool) {
	for current := directory; ; {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return current, true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return workspaceRoot(directory), false
		}
		current = parent
	}
}

// workspaceRoot is the root of the volume holding directory: "/" on Unix and
// "C:\" on Windows, the value the CLI reports outside a git worktree.
func workspaceRoot(directory string) string {
	if volume := filepath.VolumeName(directory); volume != "" {
		return volume + string(filepath.Separator)
	}
	return string(filepath.Separator)
}

// opencodeSystemPrompt assembles the full CLI system prompt for model and the
// upstream generation's tool naming. The generations order their parts
// differently: 1.x puts the model line and environment block before the skills
// listing, while 2.x lists the skills first and then the date, the environment
// and the worktree note.
func opencodeSystemPrompt(model string, v1 bool) string {
	if v1 {
		return opencodePromptV1 + opencodeEnvironmentV1(model) + "\n" + opencodeSkillsV1
	}
	return opencodePromptV2 + opencodeSkillsV2 + opencodeEnvironmentV2()
}

// injectOfficialPrompt prepends the official CLI system prompt to a shaped
// free-tier payload. It reports whether the payload changed; a body that
// already carries the marker is left untouched, so repeated shaping cannot
// duplicate the prompt. System One payloads are not chat bodies and are never
// touched.
func injectOfficialPrompt(payload map[string]any, protocol wire.Protocol, v1 bool) bool {
	if payload == nil || protocol == wire.SystemOne {
		return false
	}
	model, _ := payload["model"].(string)
	prompt := opencodeSystemPrompt(model, v1)
	switch protocol {
	case wire.Chat:
		return injectChatPrompt(payload, prompt)
	case wire.Anthropic:
		return injectAnthropicPrompt(payload, prompt)
	case wire.Responses:
		return injectResponsesPrompt(payload, prompt)
	default:
		return false
	}
}

// injectChatPrompt folds every system/developer message into the single system
// message the 1.x and 2.x CLIs send, then prepends it. Client instructions keep
// their order and are separated from the official prompt by a blank line.
func injectChatPrompt(payload map[string]any, prompt string) bool {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(messages)+1)
	var instructions []string
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" {
			kept = append(kept, raw)
			continue
		}
		content := message["content"]
		if carriesOpenCodePrompt(content) {
			return false
		}
		for _, part := range textFragments(content) {
			if part = strings.TrimSpace(part); part != "" {
				instructions = append(instructions, part)
			}
		}
	}
	system := prompt
	if joined := strings.Join(instructions, "\n"); joined != "" {
		system += "\n\n" + joined
	}
	payload["messages"] = append([]any{map[string]any{"role": "system", "content": system}}, kept...)
	return true
}

// injectAnthropicPrompt prepends the prompt to the top-level system value,
// keeping a caller's string or block list intact after it.
func injectAnthropicPrompt(payload map[string]any, prompt string) bool {
	system, exists := payload["system"]
	if exists && carriesOpenCodePrompt(system) {
		return false
	}
	if !exists {
		payload["system"] = prompt
		return true
	}
	switch value := system.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			payload["system"] = prompt
		} else {
			payload["system"] = prompt + "\n\n" + value
		}
	case []any:
		payload["system"] = append([]any{map[string]any{"type": "text", "text": prompt}}, value...)
	default:
		payload["system"] = prompt
	}
	return true
}

// injectResponsesPrompt prepends the prompt to the Responses instructions.
func injectResponsesPrompt(payload map[string]any, prompt string) bool {
	instructions, exists := payload["instructions"]
	if exists && carriesOpenCodePrompt(instructions) {
		return false
	}
	if text, ok := instructions.(string); exists && ok {
		if strings.TrimSpace(text) == "" {
			payload["instructions"] = prompt
		} else {
			payload["instructions"] = prompt + "\n\n" + text
		}
		return true
	}
	payload["instructions"] = prompt
	return true
}

// textFragments flattens a system or instruction value into its text: a plain
// string, a list of content blocks, or a block object carrying text or content.
func textFragments(value any) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var parts []string
		for _, item := range typed {
			parts = append(parts, textFragments(item)...)
		}
		return parts
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return textFragments(text)
		}
		if content, ok := typed["content"]; ok {
			return textFragments(content)
		}
	}
	return nil
}

// carriesOpenCodePrompt reports whether any text inside value already contains
// the marker.
func carriesOpenCodePrompt(value any) bool {
	for _, part := range textFragments(value) {
		if strings.Contains(part, opencodePromptMarker) || strings.Contains(part, opencodePromptMarkerV2) {
			return true
		}
	}
	return false
}
