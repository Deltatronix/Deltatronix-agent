package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A deliberately bounded JSON Schema subset; unsupported keywords fail closed.
// Only the literal-null anyOf wrapper is supported; no general combinators.
type chatSchema struct {
	Type                 json.RawMessage       `json:"type"`
	Description          string                `json:"description,omitempty"`
	Title                string                `json:"title,omitempty"`
	Properties           map[string]chatSchema `json:"properties,omitempty"`
	Required             []string              `json:"required,omitempty"`
	AdditionalProperties *bool                 `json:"additionalProperties,omitempty"`
	Items                *chatSchema           `json:"items,omitempty"`
	Enum                 []any                 `json:"enum,omitempty"`
	Const                json.RawMessage       `json:"const,omitempty"`
	Minimum              json.RawMessage       `json:"minimum,omitempty"`
	Maximum              json.RawMessage       `json:"maximum,omitempty"`
	MinLength            *int                  `json:"minLength,omitempty"`
	MaxLength            *int                  `json:"maxLength,omitempty"`
	MinItems             *int                  `json:"minItems,omitempty"`
	MaxItems             *int                  `json:"maxItems,omitempty"`
	nullable             *chatSchema
}

func (s *chatSchema) UnmarshalJSON(data []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		return errors.New("schema must be an object")
	}
	// const is a data value; every other supported keyword needs a non-null
	// operand. Check presence before Go erases null pointers/slices/defaults.
	for name, raw := range members {
		if name != "const" && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("schema keyword operand must not be null")
		}
	}
	if raw, ok := members["anyOf"]; ok {
		var branches []json.RawMessage
		if len(members) != 1 || json.Unmarshal(raw, &branches) != nil || len(branches) != 2 {
			return errors.New("unsupported nullable anyOf shape")
		}
		isNull := func(raw json.RawMessage) bool {
			var fields map[string]json.RawMessage
			var kind string
			return json.Unmarshal(raw, &fields) == nil && len(fields) == 1 && json.Unmarshal(fields["type"], &kind) == nil && kind == "null"
		}
		firstNull, secondNull := isNull(branches[0]), isNull(branches[1])
		if firstNull == secondNull {
			return errors.New("anyOf requires exactly one literal null branch")
		}
		baseIndex := 0
		if firstNull {
			baseIndex = 1
		}
		var base chatSchema
		if err := decodeChatJSON(branches[baseIndex], &base); err != nil {
			return err
		}
		kind, nullable := base.kind()
		if nullable || kind == "" || kind == "null" {
			return errors.New("anyOf base must be a single non-null type")
		}
		*s = chatSchema{nullable: &base}
		return nil
	}
	// Keep exact-member and unknown-key checks for every ordinary base schema.
	type schemaFields chatSchema
	var fields schemaFields
	if err := decodeChatJSON(data, &fields); err != nil {
		return err
	}
	*s = chatSchema(fields)
	return nil
}

// Only a single base type, or that base plus null, belongs to this subset.
func (s chatSchema) kind() (string, bool) {
	if s.nullable != nil {
		kind, _ := s.nullable.kind()
		return kind, true
	}
	var name string
	if json.Unmarshal(s.Type, &name) == nil {
		return name, false
	}
	var pair []string
	if json.Unmarshal(s.Type, &pair) != nil || len(pair) != 2 {
		return "", false
	}
	if pair[0] == "null" && pair[1] != "null" {
		return pair[1], true
	}
	if pair[1] == "null" && pair[0] != "null" {
		return pair[0], true
	}
	return "", false
}

func parseChatToolSchema(data json.RawMessage) (chatSchema, bool) {
	var schema chatSchema
	if decodeChatJSON(data, &schema) != nil {
		return schema, false
	}
	kind, nullable := schema.kind()
	return schema, kind == "object" && !nullable && schema.valid(0)
}

func (s chatSchema) valid(depth int) bool {
	if depth > 8 {
		return false
	}
	if s.nullable != nil {
		return s.nullable.valid(depth + 1)
	}
	kind, _ := s.kind()
	if !slices.Contains([]string{"object", "array", "string", "number", "integer", "boolean", "null"}, kind) || len(s.Description) > 2048 || len(s.Title) > 128 || len(s.Enum) > 64 {
		return false
	}
	if kind == "object" {
		if s.AdditionalProperties == nil || *s.AdditionalProperties || len(s.Properties) > 64 || len(s.Required) > len(s.Properties) {
			return false
		}
		seen := map[string]bool{}
		for _, key := range s.Required {
			if _, ok := s.Properties[key]; !ok || seen[key] {
				return false
			}
			seen[key] = true
		}
		for key, property := range s.Properties {
			if !validChatName(key, 128) || !property.valid(depth+1) {
				return false
			}
		}
	} else if s.Properties != nil || s.Required != nil || s.AdditionalProperties != nil {
		return false
	}
	if kind == "array" {
		if s.Items == nil || !s.Items.valid(depth+1) {
			return false
		}
	} else if s.Items != nil || s.MinItems != nil || s.MaxItems != nil {
		return false
	}
	if kind != "string" && (s.MinLength != nil || s.MaxLength != nil) {
		return false
	}
	if kind != "number" && kind != "integer" && (s.Minimum != nil || s.Maximum != nil) {
		return false
	}
	for _, v := range []*int{s.MinLength, s.MaxLength, s.MinItems, s.MaxItems} {
		if v != nil && (*v < 0 || *v > maxChatBytes) {
			return false
		}
	}
	if s.MinLength != nil && s.MaxLength != nil && *s.MinLength > *s.MaxLength {
		return false
	}
	if s.MinItems != nil && s.MaxItems != nil && *s.MinItems > *s.MaxItems {
		return false
	}
	var minimum, maximum *big.Rat
	if s.Minimum != nil {
		var ok bool
		minimum, ok = exactChatNumber(json.Number(s.Minimum))
		if !ok {
			return false
		}
	}
	if s.Maximum != nil {
		var ok bool
		maximum, ok = exactChatNumber(json.Number(s.Maximum))
		if !ok {
			return false
		}
	}
	if minimum != nil && maximum != nil && minimum.Cmp(maximum) > 0 {
		return false
	}
	return true
}

func (s chatSchema) accepts(value any, depth int) bool {
	if depth > 8 {
		return false
	}
	if s.nullable != nil {
		return value == nil || s.nullable.accepts(value, depth+1)
	}
	if s.Const != nil {
		var expected any
		if decodeChatJSON(s.Const, &expected) != nil || !sameChatSchemaValue(value, expected) {
			return false
		}
	}
	if s.Enum != nil {
		found := false
		for _, candidate := range s.Enum {
			if sameChatSchemaValue(value, candidate) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	kind, nullable := s.kind()
	if value == nil && nullable {
		return true
	}
	switch kind {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for _, key := range s.Required {
			if _, ok := object[key]; !ok {
				return false
			}
		}
		for key, v := range object {
			property, ok := s.Properties[key]
			if !ok || !property.accepts(v, depth+1) {
				return false
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok || s.Items == nil || (s.MinItems != nil && len(items) < *s.MinItems) || (s.MaxItems != nil && len(items) > *s.MaxItems) {
			return false
		}
		for _, v := range items {
			if !s.Items.accepts(v, depth+1) {
				return false
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return false
		}
		length := utf8.RuneCountInString(text)
		if (s.MinLength != nil && length < *s.MinLength) || (s.MaxLength != nil && length > *s.MaxLength) {
			return false
		}
	case "number", "integer":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		number, ok := exactChatNumber(n)
		if !ok || (kind == "integer" && !number.IsInt()) {
			return false
		}
		if s.Minimum != nil {
			minimum, ok := exactChatNumber(json.Number(s.Minimum))
			if !ok || number.Cmp(minimum) < 0 {
				return false
			}
		}
		if s.Maximum != nil {
			maximum, ok := exactChatNumber(json.Number(s.Maximum))
			if !ok || number.Cmp(maximum) > 0 {
				return false
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return false
		}
	case "null":
		if value != nil {
			return false
		}
	default:
		return false
	}
	return true
}

// Bound decimal work before allocating big integers; preserve exact JSON values.
func exactChatNumber(n json.Number) (*big.Rat, bool) {
	text := n.String()
	if len(text) == 0 || len(text) > 128 || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) || !json.Valid([]byte(text)) {
		return nil, false
	}
	mantissa := text
	if at := strings.IndexAny(text, "eE"); at >= 0 {
		exponent, err := strconv.Atoi(text[at+1:])
		if err != nil || exponent < -256 || exponent > 256 {
			return nil, false
		}
		mantissa = text[:at]
	}
	digits := 0
	for _, c := range mantissa {
		if c >= '0' && c <= '9' {
			digits++
		}
	}
	if digits > 64 {
		return nil, false
	}
	return new(big.Rat).SetString(text)
}

func sameChatSchemaValue(a, b any) bool {
	switch value := a.(type) {
	case json.Number:
		other, ok := b.(json.Number)
		if !ok {
			return false
		}
		x, ok := exactChatNumber(value)
		if !ok {
			return false
		}
		y, ok := exactChatNumber(other)
		return ok && x.Cmp(y) == 0
	case map[string]any:
		other, ok := b.(map[string]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for key, v := range value {
			o, ok := other[key]
			if !ok || !sameChatSchemaValue(v, o) {
				return false
			}
		}
		return true
	case []any:
		other, ok := b.([]any)
		if !ok || len(value) != len(other) {
			return false
		}
		for i, v := range value {
			if !sameChatSchemaValue(v, other[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validChatCall(call chatToolCall, tools []chatTool) bool {
	if call.Type != "function" || !validChatName(call.ID, 128) || len(call.Function.Arguments) > 8192 {
		return false
	}
	for _, tool := range tools {
		if tool.Function.Name != call.Function.Name {
			continue
		}
		schema, ok := parseChatToolSchema(tool.Function.Parameters)
		var args any
		return ok && decodeChatJSON([]byte(call.Function.Arguments), &args) == nil && schema.accepts(args, 0)
	}
	return false
}
