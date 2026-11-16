package claude

import "regexp"

// Model is a Claude model a workspace can run Claude Code with.
type Model struct {
	// ID is the identifier passed to `claude --model`.
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Models are the models offered when creating a workspace, most capable
// first: the current generation available through Claude Code. Which ones a
// subscription may use depends on its plan.
var Models = []Model{
	{ID: "claude-fable-5-2", Name: "Claude Fable 5.2"},
	{ID: "claude-opus-5-5", Name: "Claude Opus 5.5"},
	{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5"},
	{ID: "claude-haiku-5-5", Name: "Claude Haiku 5.5"},
}

// DefaultModel is the default model until one is chosen in the web UI.
const DefaultModel = "claude-opus-5-5"

// KnownModel reports whether id is one of Models.
func KnownModel(id string) bool {
	for _, m := range Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

var modelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

// ValidModelID reports whether id looks like a model identifier, so it is
// safe to pass to --model. It accepts IDs that are no longer in Models, so
// a workspace keeps working with the model it was created with.
func ValidModelID(id string) bool { return modelIDPattern.MatchString(id) }
