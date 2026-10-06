package chat

import "encoding/json"

// appleSchema is the guided-generation schema handed to `fm respond --schema`.
// `fm` accepts only schemas in the shape produced by `fm schema object
// (architecture checks: x-order, additionalProperties:false, required); a
// hand-written schema carrying an enum is rejected as invalid, so `effect` is
// description-guided rather than enum-enforced on the afm transport.
const appleSchema = `{
  "additionalProperties": false,
  "x-order": ["effect", "confidence", "categories", "reason"],
  "type": "object",
  "properties": {
    "effect": {
      "type": "string",
      "description": "one of: allow, ask, deny"
    },
    "confidence": {
      "type": "number",
      "description": "certainty in the effect, 0.0 to 1.0"
    },
    "categories": {
      "type": "array",
      "items": {"type": "string"},
      "description": "zero or more labels from the allowed list"
    },
    "reason": {
      "type": "string",
      "description": "one short sentence"
    }
  },
  "required": ["effect", "confidence", "categories", "reason"],
  "title": "Verdict"
}`

// httpSchema is the JSON Schema carried in response_format for the api
// transport. It constrains effect to the three effects with an enum (LM Studio
// and other OpenAI-compatible servers honour this under strict mode).
const httpSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "effect": {"type": "string", "enum": ["allow", "ask", "deny"]},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1},
    "categories": {"type": "array", "items": {"type": "string"}},
    "reason": {"type": "string"}
  },
  "required": ["effect", "confidence", "categories", "reason"]
}`

func jsonMessage(s string) json.RawMessage { return json.RawMessage(s) }
