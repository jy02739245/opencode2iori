package gateway

import (
	"strings"

	wire "opencode2api/internal/protocol"
)

type toolAdapterSpec struct {
	official     string
	fields       map[string]string // official field -> client field
	wireFields   []string
	clientSchema map[string]any
}

var clientToolAdapters = map[string]toolAdapterSpec{
	"read":  {official: "read", fields: map[string]string{"path": "file_path"}},
	"edit":  {official: "edit", fields: map[string]string{"path": "file_path", "oldString": "old_string", "newString": "new_string", "replaceAll": "replace_all"}},
	"skill": {official: "skill", fields: map[string]string{"id": "name"}},
	"write": {official: "write", fields: map[string]string{"path": "file_path"}},
}

func clientAdapterName(name string) (toolAdapterSpec, bool) {
	spec, ok := clientToolAdapters[strings.ToLower(name)]
	return spec, ok
}

// capabilityAliases maps a client tool that has no official name to the
// official tool that does the same job on each lane. The model only sees the
// official definition, so its calls must map back to the client tool and to
// that tool's argument names; otherwise the model's natural choice (the
// official name) would be a call the client cannot execute.
var capabilityAliases = map[string]struct{ v1, v2 string }{
	"exec_command": {v1: "bash", v2: "shell"},
}

// capabilityToolDefinition returns the official contract a client tool takes
// over, with the argument fields that differ between the two contracts. It
// reports false when the client tool has no official counterpart on this lane.
func capabilityToolDefinition(entry map[string]any, protocol wire.Protocol, v1 bool) (string, toolAdapterSpec, bool) {
	parameters, name, ok := toolSchema(entry, protocol)
	if !ok {
		return "", toolAdapterSpec{}, false
	}
	alias, exists := capabilityAliases[strings.ToLower(name)]
	if !exists {
		return "", toolAdapterSpec{}, false
	}
	official := alias.v2
	if v1 {
		official = alias.v1
	}
	definition, exists := officialToolDefinition(official, v1)
	if !exists {
		return "", toolAdapterSpec{}, false
	}
	spec := toolAdapterSpec{official: official, fields: map[string]string{}, clientSchema: parameters}
	officialProps, _ := definition.Parameters["properties"].(map[string]any)
	for field := range officialProps {
		spec.wireFields = append(spec.wireFields, field)
	}
	clientProps, _ := parameters["properties"].(map[string]any)
	// The official shell passes the command as "command"; a client that names
	// the same argument "cmd" (codex's exec_command) needs the rename.
	if _, hasCmd := clientProps["cmd"]; hasCmd {
		if _, hasCommand := clientProps["command"]; !hasCommand {
			spec.fields["command"] = "cmd"
		}
	}
	return name, spec, true
}

func toolSchema(entry map[string]any, protocol wire.Protocol) (map[string]any, string, bool) {
	var function map[string]any
	switch protocol {
	case wire.Chat:
		function, _ = entry["function"].(map[string]any)
		if function == nil {
			return nil, "", false
		}
		parameters, _ := function["parameters"].(map[string]any)
		name, _ := function["name"].(string)
		return parameters, name, name != ""
	case wire.Responses:
		parameters, _ := entry["parameters"].(map[string]any)
		name, _ := entry["name"].(string)
		return parameters, name, name != ""
	case wire.Anthropic:
		parameters, _ := entry["input_schema"].(map[string]any)
		name, _ := entry["name"].(string)
		return parameters, name, name != ""
	default:
		return nil, "", false
	}
}

func adaptToolDefinition(entry map[string]any, protocol wire.Protocol, v1 bool) (string, toolAdapterSpec, bool) {
	parameters, name, ok := toolSchema(entry, protocol)
	if !ok {
		return "", toolAdapterSpec{}, false
	}
	official := strings.ToLower(name)
	definition, exists := officialToolDefinition(official, v1)
	if !exists {
		return name, toolAdapterSpec{}, false
	}
	spec := toolAdapterSpec{official: official, fields: map[string]string{}, clientSchema: parameters}
	clientProps, _ := parameters["properties"].(map[string]any)
	required := map[string]bool{}
	if fields, ok := parameters["required"].([]any); ok {
		for _, field := range fields {
			if name, ok := field.(string); ok {
				required[name] = true
			}
		}
	}
	officialProps, _ := definition.Parameters["properties"].(map[string]any)
	aliases, _ := clientAdapterName(name)
	for field := range officialProps {
		spec.wireFields = append(spec.wireFields, field)
		aliasField := field
		if v1 && field == "filePath" {
			aliasField = "path"
		}
		target := aliases.fields[aliasField]
		_, hasTarget := clientProps[target]
		_, hasCanonical := clientProps[field]
		// Prefer the required client alias over an optional canonical spelling.
		if hasTarget && (!hasCanonical || required[target] && !required[field]) {
			spec.fields[field] = target
		}
	}
	return name, spec, true
}
