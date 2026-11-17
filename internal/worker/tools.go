package worker

import (
	"encoding/json"
	"slices"
)

// Mosso's tools for the agent. Claude Code reaches them through the
// worker's MCP server (see mcp.go); each call becomes a tool_call event in
// the worker's log, which the Mosso server picks up, runs on the server
// with the workspace's own GitHub credentials and repository, and answers
// with a tool_result (see calls.go). The worker only relays: it knows the
// tools' names and schemas, not how they work, and never sees a GitHub
// token.
const (
	ToolPush     = "github_push"
	ToolCreatePR = "github_create_pr"
	ToolListPRs  = "github_list_prs"
	ToolGetPR    = "github_get_pr"
	ToolUpdatePR = "github_update_pr"
	ToolChecks   = "github_checks"
	ToolFetch    = "github_fetch"
)

// Tool describes one tool to MCP clients.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// Writes says the tool changes something on GitHub.
	Writes bool `json:"-"`
}

// Tools are the tools the worker's MCP server offers.
var Tools = []Tool{
	{
		Name:  ToolPush,
		Title: "Push to GitHub",
		Description: "Push committed work from the sandbox to one of this workspace's own branches on GitHub, creating the branch if needed. " +
			"Mosso runs the push on its server with its own credentials; uncommitted changes are not pushed (commit first). " +
			"Allowed branches: the workspace's working branch, or the working branch followed by \"-\" and a short suffix (for example <working branch>-uat), one per pull request target. " +
			"Only fast-forwards: if the branch on GitHub has commits the pushed commit lacks, the push is refused (rebase onto or merge origin/<branch>, or push to a new branch). " +
			"Re-pushing an unchanged commit is a no-op.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "branch": {"type": "string", "description": "Branch on GitHub to push to: the working branch (default) or <working branch>-<suffix>."},
    "source": {"type": "string", "description": "What to push from the sandbox: HEAD (default) or the name of a local branch."}
  },
  "additionalProperties": false
}`),
		Writes: true,
	},
	{
		Name:  ToolCreatePR,
		Title: "Create a pull request",
		Description: "Open a pull request from one of this workspace's branches (already pushed with github_push) into a base branch of the repository. " +
			"If an open pull request from that head into that base exists, it is returned instead (\"created\": false), so calling this again is safe. " +
			"Use a different head branch for each base branch.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "Pull request title."},
    "body": {"type": "string", "description": "Pull request description (Markdown)."},
    "base": {"type": "string", "description": "Branch to merge into; defaults to the workspace's base branch."},
    "head": {"type": "string", "description": "This workspace's branch with the changes; defaults to the working branch."},
    "draft": {"type": "boolean", "description": "Open as a draft. Default false."}
  },
  "required": ["title"],
  "additionalProperties": false
}`),
		Writes: true,
	},
	{
		Name:        ToolListPRs,
		Title:       "List this workspace's pull requests",
		Description: "List the pull requests whose head is one of this workspace's branches, with their state, base and URL.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "state": {"type": "string", "enum": ["open", "closed", "all"], "description": "Which pull requests; default all."}
  },
  "additionalProperties": false
}`),
	},
	{
		Name:        ToolGetPR,
		Title:       "Read a pull request",
		Description: "Read one of this workspace's pull requests: title, description, state, draft, whether it is merged or mergeable, head and base.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "number": {"type": "integer", "minimum": 1, "description": "Pull request number."}
  },
  "required": ["number"],
  "additionalProperties": false
}`),
	},
	{
		Name:        ToolUpdatePR,
		Title:       "Update a pull request",
		Description: "Change the title, description or draft status of one of this workspace's open pull requests. Only the fields given change. It cannot merge, close or retarget a pull request.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "number": {"type": "integer", "minimum": 1, "description": "Pull request number."},
    "title": {"type": "string", "description": "New title."},
    "body": {"type": "string", "description": "New description (Markdown); replaces the old one."},
    "draft": {"type": "boolean", "description": "true to convert to a draft, false to mark ready for review."}
  },
  "required": ["number"],
  "additionalProperties": false
}`),
		Writes: true,
	},
	{
		Name:  ToolChecks,
		Title: "Read CI results",
		Description: "Read the CI results (check runs and commit statuses) of the head commit of one of this workspace's pull requests, or of a branch. " +
			"The summary state is success, failure, pending, none, or unknown. \"complete\": false means some results could not be read (see missing): then success is never claimed, and the state is unknown unless a failure or pending check is visible. " +
			"Checks take time after a push: if pending, wait and ask again rather than polling quickly.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pr": {"type": "integer", "minimum": 1, "description": "One of this workspace's pull requests."},
    "branch": {"type": "string", "description": "Instead of pr: a branch of the repository."}
  },
  "additionalProperties": false
}`),
	},
	{
		Name:  ToolFetch,
		Title: "Fetch a branch from GitHub",
		Description: "Fetch a branch of the repository from GitHub into the sandbox as origin/<branch> (git fetch does not work here), and keep it current before each turn. " +
			"Use it to base work on another branch, for example to prepare a pull request into it.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "branch": {"type": "string", "description": "Branch name, for example uat."}
  },
  "required": ["branch"],
  "additionalProperties": false
}`),
	},
}

// FindTool returns the tool called name.
func FindTool(name string) (Tool, bool) {
	i := slices.IndexFunc(Tools, func(t Tool) bool { return t.Name == name })
	if i < 0 {
		return Tool{}, false
	}
	return Tools[i], true
}

// Arguments of the tools, as the Mosso server decodes them.
type (
	PushArgs struct {
		Branch string `json:"branch"`
		Source string `json:"source"`
	}
	CreatePRArgs struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Base  string `json:"base"`
		Head  string `json:"head"`
		Draft bool   `json:"draft"`
	}
	ListPRsArgs struct {
		State string `json:"state"`
	}
	GetPRArgs struct {
		Number int `json:"number"`
	}
	UpdatePRArgs struct {
		Number int     `json:"number"`
		Title  *string `json:"title"`
		Body   *string `json:"body"`
		Draft  *bool   `json:"draft"`
	}
	ChecksArgs struct {
		PR     int    `json:"pr"`
		Branch string `json:"branch"`
	}
	FetchArgs struct {
		Branch string `json:"branch"`
	}
)
