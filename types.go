package muninn

import (
	"errors"
	"time"
)

// ErrNotFound is returned when an engram with the requested ID does not exist.
var ErrNotFound = errors.New("engram not found")

// Engram is a single memory record returned by the public API.
type Engram struct {
	ID         string    `json:"id"`
	Concept    string    `json:"concept"`
	Content    string    `json:"content"`
	Summary    string    `json:"summary,omitempty"`
	State      string    `json:"state,omitempty"`
	Score      float64   `json:"score,omitempty"`
	Confidence float32   `json:"confidence,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastAccess time.Time `json:"last_access"`
}

// MemoryType is a rule-based classification for engrams.
type MemoryType uint8

const (
	TypeFact        MemoryType = 0  // factual information
	TypeDecision    MemoryType = 1  // choices made with rationale
	TypeObservation MemoryType = 2  // something noticed, insight
	TypePreference  MemoryType = 3  // opinions, personal choices
	TypeIssue       MemoryType = 4  // bugs, problems, defects
	TypeTask        MemoryType = 5  // action items, to-dos
	TypeProcedure   MemoryType = 6  // how-to, workflows, processes
	TypeEvent       MemoryType = 7  // something that happened, temporal
	TypeGoal        MemoryType = 8  // objectives, targets, intentions
	TypeConstraint  MemoryType = 9  // rules, limitations, requirements
	TypeIdentity    MemoryType = 10 // about a person, role, entity
	TypeReference   MemoryType = 11 // documentation, specifications
)

// ParseMemoryType parses a string into a MemoryType.
// Accepts both MuninnDB canonical names (fact, decision, …) and common
// cognitive-science aliases (episodic, semantic, declarative, …).
// Returns TypeFact and false if the string is not recognised.
func ParseMemoryType(s string) (MemoryType, bool) {
	switch s {
	// MuninnDB canonical names
	case "fact":
		return TypeFact, true
	case "decision":
		return TypeDecision, true
	case "observation":
		return TypeObservation, true
	case "preference":
		return TypePreference, true
	case "issue", "bugfix", "bug_report":
		return TypeIssue, true
	case "task":
		return TypeTask, true
	case "procedure", "procedural":
		return TypeProcedure, true
	case "event", "experience":
		return TypeEvent, true
	case "goal":
		return TypeGoal, true
	case "constraint":
		return TypeConstraint, true
	case "identity":
		return TypeIdentity, true
	case "reference":
		return TypeReference, true
	// Cognitive-science aliases (aimemkb memory types)
	case "episodic":
		return TypeEvent, true
	case "semantic", "declarative":
		return TypeFact, true
	case "working":
		return TypeObservation, true
	case "spatial":
		return TypeReference, true
	case "sensory", "flash":
		return TypeEvent, true
	case "autobiographical":
		return TypeIdentity, true
	case "prospective":
		return TypeGoal, true
	case "implicit":
		return TypePreference, true
	case "emotional":
		return TypeObservation, true
	default:
		return TypeFact, false
	}
}

// EnrichConfig configures the optional retroactive LLM-based enrichment.
type EnrichConfig struct {
	ProviderURL string // e.g. "ollama://qwen2.5:7b@localhost:11434"
	APIKey      string // for cloud providers; empty for Ollama
}

// Entity is a named entity extracted from memory content.
type Entity struct {
	Name string `json:"name"`
	Type string `json:"type"` // person, organization, project, tool, ...
}

// EntityRelationship is a typed relationship between two entities.
type EntityRelationship struct {
	FromEntity string  `json:"from_entity"`
	ToEntity   string  `json:"to_entity"`
	RelType    string  `json:"rel_type"` // manages, uses, depends_on, ...
	Weight     float32 `json:"weight"`   // 0.0–1.0
}

// EnrichedMemory holds pre-computed enrichment data that can be passed to
// [DB.RememberEnriched] so that MuninnDB's background enrichment pipeline
// skips the corresponding stages.
type EnrichedMemory struct {
	Concept    string   // short label (required)
	Content    string   // full text (required)
	MemoryType MemoryType
	TypeLabel  string   // free-form label, e.g. "meeting_notes"
	Summary    string
	Tags       []string
	Confidence float32

	Entities            []Entity
	EntityRelationships []EntityRelationship
}
