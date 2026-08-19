package schema

import (
	"strings"
	"testing"
)

const storySpecSchema = `{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"type": "object",
	"required": ["title", "beats"],
	"properties": {
		"title": {"type": "string"},
		"beats": {"type": "array", "items": {"type": "string"}, "minItems": 1}
	}
}`

func TestCompileValidSchema(t *testing.T) {
	v, err := Compile([]byte(storySpecSchema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if v.Raw() == nil {
		t.Fatal("expected Raw() to return the compiled schema")
	}
}

func TestCompileRejectsMalformedSchema(t *testing.T) {
	if _, err := Compile([]byte(`{not json`)); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestCompileRejectsUnsupportedDraft(t *testing.T) {
	// draft-04 has no "$schema"-based type keyword conflicts with 2020-12
	// that would fail at Compile time (the library resolves structurally
	// first) — the version check happens at Validate, so assert there.
	v, err := Compile([]byte(`{"$schema": "http://json-schema.org/draft-04/schema#", "type": "object"}`))
	if err != nil {
		// Also acceptable: rejected up front at Compile.
		return
	}
	if problems := v.Validate([]byte(`{}`)); len(problems) == 0 {
		t.Fatal("expected an unsupported-draft schema to fail validation")
	} else if !strings.Contains(problems[0], "supported versions") {
		t.Fatalf("expected an unsupported-version error, got %q", problems[0])
	}
}

func TestValidateAcceptsConformingJSON(t *testing.T) {
	v, err := Compile([]byte(storySpecSchema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if problems := v.Validate([]byte(`{"title":"x","beats":["a","b"]}`)); problems != nil {
		t.Fatalf("expected no problems, got %v", problems)
	}
}

func TestValidateReportsMissingRequired(t *testing.T) {
	v, err := Compile([]byte(storySpecSchema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	problems := v.Validate([]byte(`{"title":"x"}`))
	if len(problems) == 0 {
		t.Fatal("expected a violation for the missing \"beats\" property")
	}
	if !strings.Contains(problems[0], "beats") {
		t.Fatalf("expected the error to mention the missing property, got %q", problems[0])
	}
}

func TestValidateReportsTypeMismatch(t *testing.T) {
	v, err := Compile([]byte(storySpecSchema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	problems := v.Validate([]byte(`{"title":"x","beats":"not an array"}`))
	if len(problems) == 0 {
		t.Fatal("expected a violation for the wrong type")
	}
}

func TestValidateReportsInvalidJSON(t *testing.T) {
	v, err := Compile([]byte(storySpecSchema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	problems := v.Validate([]byte(`not json`))
	if len(problems) == 0 {
		t.Fatal("expected a violation for non-JSON input")
	}
}

func TestInstructionEmbedsSchema(t *testing.T) {
	v, err := Compile([]byte(storySpecSchema))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	instr := Instruction(v.Raw())
	if !strings.Contains(instr, "JSON Schema") {
		t.Fatalf("expected the instruction to mention JSON Schema, got %q", instr)
	}
	if !strings.Contains(instr, "\"title\"") {
		t.Fatalf("expected the instruction to embed the schema itself, got %q", instr)
	}
}
