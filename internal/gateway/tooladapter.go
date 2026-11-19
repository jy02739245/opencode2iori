package gateway

import (
	"strings"

	wire "opencode2api/internal/protocol"
)

type toolAdapterSpec struct {
	official   string
	fields     map[string]string // official field -> client field
	wireFields []string
}

var clientToolAdapters = map[string]toolAdapterSpec{
	"read":  {official: "read", fields: map[string]string{"path": "file_path"}},
	"edit":  {official: "edit", fields: map[string]string{"path": "file_path", "oldString": "old_string", "newString": "new_string", "replaceAll": "replace_all"}},
	"skill": {official: "skill", fields: map[string]string{"id": "name"}},
}

func clientAdapterName(name string) (toolAdapterSpec, bool) {
	spec, ok := clientToolAdapters[strings.ToLower(name)]
	return spec, ok
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
	spec, matched := clientAdapterName(name)
	if !matched {
		return "", toolAdapterSpec{}, false
	}
	definition, exists := officialToolDefinition(spec.official, v1)
	if !exists || !compatibleAdapterSchema(parameters, definition.Parameters, spec.fields) {
		return name, toolAdapterSpec{}, false
	}
	for field := range definition.Parameters["properties"].(map[string]any) {
		spec.wireFields = append(spec.wireFields, field)
	}
	return name, spec, true
}

func compatibleAdapterSchema(client, official map[string]any, fields map[string]string) bool {
	if client == nil || official == nil || client["type"] != "object" || official["type"] != "object" {
		return false
	}
	clientProps, _ := client["properties"].(map[string]any)
	officialProps, _ := official["properties"].(map[string]any)
	if clientProps == nil || officialProps == nil {
		return false
	}
	// Root conditionals can hide required fields that a flat key adapter cannot honor.
	for _, keyword := range []string{"$ref", "$dynamicRef", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "dependencies", "dependentRequired", "dependentSchemas", "patternProperties", "propertyNames"} {
		if _, exists := client[keyword]; exists {
			return false
		}
	}
	clientRequired, ok := stringSet(client["required"])
	if !ok {
		return false
	}
	officialRequired, ok := stringSet(official["required"])
	if !ok {
		return false
	}
	for officialName, officialProperty := range officialProps {
		clientName := officialName
		if mapped, exists := fields[officialName]; exists {
			clientName = mapped
		}
		if officialName != clientName {
			if _, collision := clientProps[officialName]; collision {
				return false
			}
		}
		clientProperty, exists := clientProps[clientName]
		if !exists {
			if officialRequired[officialName] {
				return false
			}
			continue
		}
		clientProp, cOK := clientProperty.(map[string]any)
		officialProp, oOK := officialProperty.(map[string]any)
		if !cOK || !oOK || !schemaTypesCompatible(clientProp["type"], officialProp["type"]) {
			return false
		}
	}
	for field := range officialRequired {
		clientField, mapped := fields[field]
		if !mapped || !clientRequired[clientField] {
			return false
		}
	}
	for field := range clientRequired {
		mapped := false
		for officialField, clientField := range fields {
			if field == clientField && officialRequired[officialField] {
				mapped = true
				break
			}
		}
		if !mapped {
			return false
		}
	}
	return true
}

func schemaTypesCompatible(client, official any) bool {
	clientType, clientOK := client.(string)
	officialType, officialOK := official.(string)
	if !clientOK || !officialOK {
		return false
	}
	return clientType == officialType || (clientType == "number" && officialType == "integer") || (clientType == "integer" && officialType == "number")
}

func stringSet(value any) (map[string]bool, bool) {
	if value == nil {
		return map[string]bool{}, true
	}
	values, ok := value.([]any)
	if !ok {
		return nil, false
	}
	set := make(map[string]bool, len(values))
	for _, value := range values {
		name, ok := value.(string)
		if !ok || name == "" {
			return nil, false
		}
		set[name] = true
	}
	return set, true
}
