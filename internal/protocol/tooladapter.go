package protocol

import (
	"bytes"
	"encoding/json"
)

type toolArgumentAdapter struct {
	toClient     map[string]string
	toWire       map[string]string
	wireFields   map[string]bool
	clientFields map[string]bool
}

// BindAdapter registers a tool contract and its top-level wire-to-client fields.
// Identity names are registered too: arguments can differ even when names match.
// Optional wireFields limit outbound history to the official contract's properties.
func (r *ToolRenames) BindAdapter(client, wire string, fields map[string]string, wireFields ...string) bool {
	if r == nil || client == "" || wire == "" {
		return false
	}
	if existing, ok := r.Wire(client); ok && existing != wire {
		return false
	}
	if existing, ok := r.Client(wire); ok && existing != client {
		return false
	}
	adapter := toolArgumentAdapter{toClient: map[string]string{}, toWire: map[string]string{}}
	for source, target := range fields {
		if source == "" || target == "" {
			return false
		}
		if previous, exists := adapter.toWire[target]; exists && previous != source {
			return false
		}
		adapter.toClient[source] = target
		adapter.toWire[target] = source
	}
	if len(wireFields) > 0 {
		adapter.wireFields = make(map[string]bool, len(wireFields))
		for _, field := range wireFields {
			adapter.wireFields[field] = true
		}
	}
	r.Add(client, wire)
	r.BindOfficial(wire, client)
	if r.adapters == nil {
		r.adapters = map[string]toolArgumentAdapter{}
	}
	r.adapters[wire] = adapter
	return true
}

// BindClientSchema retains closed client properties for returned arguments only.
// Schemas allowing additional properties keep those values, as JSON Schema requires.
func (r *ToolRenames) BindClientSchema(wire string, schema map[string]any) {
	if r == nil {
		return
	}
	adapter, exists := r.adapters[wire]
	if !exists {
		return
	}
	adapter.clientFields = nil
	if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
		properties, _ := schema["properties"].(map[string]any)
		adapter.clientFields = make(map[string]bool, len(properties))
		for field := range properties {
			adapter.clientFields[field] = true
		}
	}
	r.adapters[wire] = adapter
}

// HasAdapters reports whether this request has any argument contracts to adapt.
func (r *ToolRenames) HasAdapters() bool {
	return r != nil && len(r.adapters) > 0
}

// HasArgumentAdapter only matches registered wire names, never an inferred alias.
func (r *ToolRenames) HasArgumentAdapter(wire string) bool {
	if r == nil {
		return false
	}
	_, ok := r.adapters[wire]
	return ok
}

// ToClientArguments converts only the registered upstream contract's root keys.
func (r *ToolRenames) ToClientArguments(wire string, arguments any) (any, bool) {
	if r == nil {
		return arguments, false
	}
	adapter, ok := r.adapters[wire]
	if !ok {
		return arguments, false
	}
	return translateArgumentKeys(arguments, adapter.toClient, adapter.clientFields)
}

// ToWireArguments converts client history back to the contract shown upstream.
func (r *ToolRenames) ToWireArguments(name string, arguments any) (any, bool) {
	if r == nil {
		return arguments, false
	}
	wire := name
	if mapped, ok := r.Wire(name); ok {
		wire = mapped
	}
	adapter, ok := r.adapters[wire]
	if !ok {
		return arguments, false
	}
	return translateArgumentKeys(arguments, adapter.toWire, adapter.wireFields)
}

func translateArgumentKeys(arguments any, fields map[string]string, allowed map[string]bool) (any, bool) {
	var object map[string]any
	text, encoded := arguments.(string)
	if encoded {
		// Keep large integers and nested values exact while rewriting JSON keys.
		decoder := json.NewDecoder(bytes.NewBufferString(text))
		decoder.UseNumber()
		if !json.Valid([]byte(text)) || decoder.Decode(&object) != nil || object == nil {
			return arguments, false
		}
	} else {
		var ok bool
		object, ok = arguments.(map[string]any)
		if !ok || object == nil {
			return arguments, false
		}
	}
	changed := false
	for source, target := range fields {
		if source == target {
			continue
		}
		if _, exists := object[source]; !exists {
			continue
		}
		// A conflicting contract is left intact for validation, never overwritten.
		if _, collision := object[target]; collision {
			return arguments, false
		}
		changed = true
	}
	if allowed != nil {
		for key := range object {
			if target, mapped := fields[key]; mapped {
				key = target
			}
			if !allowed[key] {
				changed = true
			}
		}
	}
	if !changed {
		return arguments, false
	}
	converted := make(map[string]any, len(object))
	for key, value := range object {
		if target, ok := fields[key]; ok {
			key = target
		}
		if allowed == nil || allowed[key] {
			converted[key] = value
		}
	}
	if !encoded {
		return converted, true
	}
	result, err := json.Marshal(converted)
	if err != nil {
		return arguments, false
	}
	return string(result), true
}
