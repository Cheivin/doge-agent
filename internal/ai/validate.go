package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaCache caches compiled tool parameter schemas. Schemas are keyed by
// their JSON text so that tools with the same schema are compiled once.
var schemaCache sync.Map // map[string]*jsonschema.Schema

// compileSchema compiles a JSON Schema document (draft 2020-12).
func compileSchema(schema any) (*jsonschema.Schema, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	if cached, ok := schemaCache.Load(string(data)); ok {
		return cached.(*jsonschema.Schema), nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("schema.json", doc); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	sch, err := c.Compile("schema.json")
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	schemaCache.Store(string(data), sch)
	return sch, nil
}

// ValidateToolArguments validates raw tool arguments against the tool's
// parameter schema, following the validateToolArguments.
//
// Models that stream arguments as a JSON string are handled by parsing the
// string first. The validated value is returned; it is guaranteed to conform
// to the schema (a *ValidationError is returned otherwise).
func ValidateToolArguments(tool *Tool, args any) (any, error) {
	var v any
	switch a := args.(type) {
	case string:
		if err := json.Unmarshal([]byte(a), &v); err != nil {
			return nil, fmt.Errorf("arguments must be a valid JSON object: %w", err)
		}
	case nil:
		v = map[string]any{}
	default:
		v = a
	}
	if tool == nil {
		return v, nil
	}
	schema, err := compileSchema(tool.Parameters)
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(v); err != nil {
		return nil, formatValidationError(err)
	}
	return v, nil
}

func formatValidationError(err error) error {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	msg := describeValidationError(ve)
	loc := ve.InstanceLocation
	if len(loc) > 0 {
		return fmt.Errorf("参数校验失败: at %s: %s", strings.Join(loc, "."), msg)
	}
	return fmt.Errorf("参数校验失败: %s", msg)
}

// describeValidationError produces a concise message, dropping the schema
// URL noise from jsonschema ("jsonschema validation failed with 'file://…#'").
func describeValidationError(ve *jsonschema.ValidationError) string {
	// Recurse to the deepest cause for the most specific message.
	if len(ve.Causes) > 0 {
		return describeValidationError(ve.Causes[0])
	}
	msg := ve.Error()
	// Strip the schema-URL prefix if present.
	if idx := strings.Index(msg, "': "); idx >= 0 {
		return msg[idx+3:]
	}
	if idx := strings.Index(msg, "':"); idx >= 0 {
		return msg[idx+2:]
	}
	return msg
}
