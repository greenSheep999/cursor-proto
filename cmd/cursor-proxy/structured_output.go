package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

type structuredOutputFormat struct {
	Type       string          `json:"type"`
	Name       string          `json:"name"`
	Strict     bool            `json:"strict"`
	Schema     json.RawMessage `json:"schema"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Strict bool            `json:"strict"`
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema"`
}

func appendResponsesStructuredOutputInstruction(systemPrompt string, textConfig json.RawMessage) string {
	if len(textConfig) == 0 || string(textConfig) == "null" {
		return systemPrompt
	}
	var config struct {
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(textConfig, &config); err != nil {
		return systemPrompt
	}
	return appendStructuredOutputInstruction(systemPrompt, config.Format)
}

func appendStructuredOutputInstruction(systemPrompt string, raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return systemPrompt
	}
	var format structuredOutputFormat
	if err := json.Unmarshal(raw, &format); err != nil {
		return systemPrompt
	}
	if format.Type != "json_object" && format.Type != "json_schema" {
		return systemPrompt
	}
	instruction := "Return only valid JSON with no markdown fence, commentary, or trailing text."
	var schema json.RawMessage
	if format.JSONSchema != nil {
		schema = format.JSONSchema.Schema
	} else {
		schema = format.Schema
	}
	if len(schema) > 0 && string(schema) != "null" {
		var compact bytes.Buffer
		if json.Compact(&compact, schema) == nil {
			instruction += " The JSON must strictly satisfy this schema: " + compact.String()
		}
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return instruction
	}
	return strings.TrimSpace(systemPrompt) + "\n" + instruction
}
