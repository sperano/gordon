// Package profile resolves a run's execution profile (agent, model,
// effort, ServiceAccount, credentials and named capabilities) from trusted
// configuration only. Nothing in an event payload can name a
// ServiceAccount, a Secret or a pod override: a request names a project,
// a variant and a mode, and at most a named profile the project's
// configuration allows for that mode.
package profile

// Mode is a run's permission mode.
type Mode string

const (
	ModeImplement Mode = "implement"
	ModeDiscuss   Mode = "discuss"
	ModeFollowUp  Mode = "followup"
	ModeAnswer    Mode = "answer"
	ModeReview    Mode = "review"
)

// Capability is a named grant a profile carries. Gordon 02 binds each one
// to its ServiceAccount, Secrets, database role and network policy.
type Capability string

const (
	// GitHubWrite is the Workspace's GitHub App credential and, for a run
	// on a branch, the PR opener sidecar.
	GitHubWrite Capability = "github-write"
	// GitHubReadOnly is the clone-only PAT of a `<workspace>-readonly`
	// Workspace (kelos-github-readonly).
	GitHubReadOnly Capability = "github-readonly"
	// ImageBuildSubmit is the image-build dispatch key and the
	// image-build-submitter Role.
	ImageBuildSubmit Capability = "image-build-submit"
	// ClusterRead is the kelos-discuss read ClusterRole.
	ClusterRead Capability = "cluster-read"
	// PostgresTest is the pod-local throwaway test Postgres sidecar.
	PostgresTest Capability = "postgres-test"
	// PuckDBProdReadOnly is the puckdb_readonly role on the
	// shared-postgres replica (Secret puckdb-prod-readonly).
	PuckDBProdReadOnly Capability = "puckdb-prod-readonly"
	// Mactest is the mactest client certificate (Secret mactest-client).
	Mactest Capability = "mactest"
)

// writes classifies every known capability: true when it can change
// something outside the pod. A capability missing here is unknown.
var writes = map[Capability]bool{
	GitHubWrite: true, ImageBuildSubmit: true,
	GitHubReadOnly: false, ClusterRead: false, PostgresTest: false, PuckDBProdReadOnly: false, Mactest: false,
}

// Writes reports whether the capability can change state outside the
// pod; known is false for a capability with no classification, which
// checkProfile refuses so a new grant fails closed.
func (c Capability) Writes() (writing, known bool) {
	writing, known = writes[c]
	return writing, known
}

// Runner is one agent runtime (vikunjaSpawners.runners).
type Runner struct {
	Type             string   `json:"type"`
	Image            string   `json:"image"`
	CredentialType   string   `json:"credential_type"`
	CredentialSecret string   `json:"credential_secret,omitempty"`
	Efforts          []string `json:"efforts"`
}

// Variant is one assignable agent (vikunjaSpawners.variants): its name
// is the Vikunja agent user and the `kelos:<name>` PR label.
type Variant struct {
	Runner string `json:"runner"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// Profile is a named execution capability profile.
type Profile struct {
	ServiceAccount string       `json:"service_account"`
	ReadOnly       bool         `json:"read_only"`
	Capabilities   []Capability `json:"capabilities"`
}

// Project is one Vikunja project and the Workspace its runs use.
type Project struct {
	Name string `json:"name"`
	// Title is the Vikunja project's display name the prompts quote.
	Title        string   `json:"title,omitempty"`
	VikunjaID    int64    `json:"vikunja_id"`
	Workspace    string   `json:"workspace"`
	Repository   string   `json:"repository"`
	AgentConfigs []string `json:"agent_configs,omitempty"`
	// Profiles maps a mode to its default profile name; Allowed lists
	// further profiles a trusted override may pick per mode.
	Profiles map[Mode]string   `json:"profiles"`
	Allowed  map[Mode][]string `json:"allowed,omitempty"`
}

// Reviewer is one PR reviewer (vikunjaSpawners.prReview.reviewers).
type Reviewer struct {
	Name         string   `json:"name"`
	Variant      string   `json:"variant"`
	Effort       string   `json:"effort"`
	FilePatterns []string `json:"file_patterns,omitempty"`
}

// Config is Gordon's trusted routing configuration.
type Config struct {
	// Version names the configuration; each run records it.
	Version        string              `json:"version"`
	DefaultVariant string              `json:"default_variant"`
	Runners        map[string]Runner   `json:"runners"`
	Variants       map[string]Variant  `json:"variants"`
	Profiles       map[string]Profile  `json:"profiles"`
	Projects       []Project           `json:"projects"`
	ReviewProfile  string              `json:"review_profile"`
	CommonReviewer []Reviewer          `json:"common_reviewers"`
	StackReviewers map[string]Reviewer `json:"stack_reviewers"`
	Policy         Policy              `json:"policy"`
}

// Policy holds the labels, buckets and identities routing keys on.
type Policy struct {
	VikunjaOwner        string   `json:"vikunja_owner"`
	GitHubOwner         string   `json:"github_owner"`
	Trigger             string   `json:"trigger"`
	PullRequestAuthors  []string `json:"pull_request_authors"`
	ReviewSkipAuthors   []string `json:"review_skip_authors"`
	InProgressLabel     string   `json:"in_progress_label"`
	DiscussLabel        string   `json:"discuss_label"`
	NoAgentLabel        string   `json:"no_agent_label"`
	ChainLabel          string   `json:"chain_label"`
	PROpenLabel         string   `json:"pr_open_label"`
	MergeCloseLabels    []string `json:"merge_close_labels"`
	CloseLabels         []string `json:"close_labels"`
	DoingBucket         string   `json:"doing_bucket"`
	ReviewBucket        string   `json:"review_bucket"`
	VariantLabelPrefix  string   `json:"variant_label_prefix"`
	DraftLabel          string   `json:"draft_label"`
	FixSuggestionsLabel string   `json:"fix_suggestions_label"`
	NoReviewLabel       string   `json:"no_review_label"`
	ReviewBots          []Bot    `json:"review_bots"`
}

// Bot is a review bot whose findings start a PR follow-up.
type Bot struct {
	Login              string `json:"login"`
	FindingsPattern    string `json:"findings_pattern"`
	SuggestionsPattern string `json:"suggestions_pattern,omitempty"`
}

// Project returns the project of a Vikunja project id.
func (c Config) Project(vikunjaID int64) (Project, bool) {
	for _, p := range c.Projects {
		if p.VikunjaID == vikunjaID {
			return p, true
		}
	}
	return Project{}, false
}

// PRProject returns the project whose Workspace takes a repository's pull
// requests: the first project on that repository, as kelos.prWorkspace
// picks the first Workspace on a repository.
func (c Config) PRProject(repository string) (Project, bool) {
	for _, p := range c.Projects {
		if p.Repository == repository {
			return p, true
		}
	}
	return Project{}, false
}

// IsAgent reports whether a Vikunja username is an agent variant.
func (c Config) IsAgent(username string) bool {
	_, ok := c.Variants[username]
	return ok
}
