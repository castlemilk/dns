package simplemcp

import (
	"encoding/json"
	"errors"
	"testing"
)

// text, object and list read one field out of a decoded JSON answer and fail
// the test when it is not the shape the assertion assumes. They exist so no
// test needs an unchecked type assertion.
func text(t *testing.T, document map[string]any, key string) string {
	t.Helper()
	value, ok := document[key].(string)
	if !ok {
		t.Fatalf("%q is %v, want a string", key, document[key])
	}
	return value
}

func object(t *testing.T, value any) map[string]any {
	t.Helper()
	fields, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", value)
	}
	return fields
}

func list(t *testing.T, value any) []any {
	t.Helper()
	values, ok := value.([]any)
	if !ok {
		t.Fatalf("%v is not an array", value)
	}
	return values
}

// toolNamed looks a tool up and fails when the registry has lost it.
func toolNamed(t *testing.T, name string) tool {
	t.Helper()
	definition, found := lookupTool(name)
	if !found {
		t.Fatalf("%s is not registered", name)
	}
	return definition
}

// mustJSON encodes a test argument the way a client would send it.
func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %v: %v", value, err)
	}
	return encoded
}

// asToolError is errors.As with the test's target, kept here so the assertion
// reads the same in every test that makes it.
func asToolError(err error, target **toolError) bool {
	return errors.As(err, target)
}
