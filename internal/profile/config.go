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
	// ModeOps is an implementation run that may change the live cluster
	// (the legacy clusterOperator mode): its profile is the configuration's
	// OpsProfile, never a project's.
	ModeOps Mode = "ops"
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
	// NamespaceRead is the same read in a namespaced reader account's
	// namespaces only.
	NamespaceRead Capability = "namespace-read"
	// PostgresTest is the pod-local throwaway test Postgres sidecar.
	PostgresTest Capability = "postgres-test"
	// PuckDBProdReadOnly is the puckdb_readonly role on the
	// shared-postgres replica (Secret puckdb-prod-readonly).
	PuckDBProdReadOnly Capability = "puckdb-prod-readonly"
	// Mactest is the mactest client certificate (Secret mactest-client).
	Mactest Capability = "mactest"
	// ClusterWrite is the cluster operator's write ClusterRole: only the
	// ops profile may grant it, and only to an ops run.
	ClusterWrite Capability = "cluster-write"
)

// writes classifies every known capability: true when it can change
// something outside the pod. A capability missing here is unknown.
var writes = map[Capability]bool{
	GitHubWrite: true, ImageBuildSubmit: true, ClusterWrite: true,
	GitHubReadOnly: false, ClusterRead: false, NamespaceRead: false, PostgresTest: false, PuckDBProdReadOnly: false, Mactest: false,
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
	Title      string `json:"title,omitempty"`
	VikunjaID  int64  `json:"vikunja_id"`
	Workspace  string `json:"workspace"`
	Repository string `json:"repository"`
	// BaseRef is the Workspace's ref, the base its pull requests merge
	// into ("" for DefaultBaseRef).
	BaseRef      string   `json:"base_ref,omitempty"`
	AgentConfigs []string `json:"agent_configs,omitempty"`
	// AutoAssignVariant is the agent username assigned to new unassigned
	// tasks in this project; empty disables automatic assignment.
	AutoAssignVariant string `json:"auto_assign_variant,omitempty"`
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
	Version        string             `json:"version"`
	DefaultVariant string             `json:"default_variant"`
	Runners        map[string]Runner  `json:"runners"`
	Variants       map[string]Variant `json:"variants"`
	Profiles       map[string]Profile `json:"profiles"`
	Projects       []Project          `json:"projects"`
	ReviewProfile  string             `json:"review_profile"`
	// OpsProfile is the ops mode's profile (executionProfiles.system.ops),
	// "" while the ops mode is off.
	OpsProfile     string              `json:"ops_profile,omitempty"`
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
	// The ops mode (clusterOperator): a task of an OpsProjects project
	// carrying OpsLabel, created by OpsOwner and started by OpsOwner,
	// gets an ops run instead of its implementation run. An empty
	// OpsLabel turns the mode off.
	OpsLabel    string   `json:"ops_label,omitempty"`
	OpsOwner    string   `json:"ops_owner,omitempty"`
	OpsProjects []string `json:"ops_projects,omitempty"`
	// The Vikunja lifecycle (Gordon 06): the bucket a stopped card goes
	// back to, the labels a start clears and a failure sets, the chain's
	// auto-merge label and its trusted creator, Gordon's own Vikunja
	// user (only its comments count as notices) and the web base links
	// point at.
	BacklogBucket         string `json:"backlog_bucket,omitempty"`
	AgentErrorLabel       string `json:"agent_error_label,omitempty"`
	ActionNeededLabel     string `json:"action_needed_label,omitempty"`
	AutoMergeLabel        string `json:"auto_merge_label,omitempty"`
	AutoMergeLabelCreator string `json:"auto_merge_label_creator,omitempty"`
	VikunjaServiceUser    string `json:"vikunja_service_user,omitempty"`
	VikunjaWebURL         string `json:"vikunja_web_url,omitempty"`
	// PRReadiness is pr_open's PR_READINESS_ENABLED: the pr-open notice
	// promises a ready-to-merge notice.
	PRReadiness bool `json:"pr_readiness,omitempty"`
	// CancelReaction is the owner's reaction that withdraws a `/gordon`
	// request: the reconciliation never recovers it.
	CancelReaction string `json:"cancel_reaction,omitempty"`
	// The review-round caps (cap_review_rounds.py), 0 for none: the
	// review-bot rounds a pull request may take (followUp.maxRoundsPerPR),
	// and the pushes whose suggestions-only reviews may start an agent
	// (followUp.fixSuggestions.maxRounds).
	MaxRoundsPerPR       int `json:"max_rounds_per_pr,omitempty"`
	SuggestionsMaxRounds int `json:"suggestions_max_rounds,omitempty"`
	// The PR readiness verdict labels (labels.readyToMerge,
	// labels.roundLimitReached), "" for none, and the readiness gate.
	ReadyToMergeLabel string    `json:"ready_to_merge_label,omitempty"`
	RoundLimitLabel   string    `json:"round_limit_label,omitempty"`
	Readiness         Readiness `json:"readiness,omitzero"`
}

// Readiness is the PR readiness gate (vikunjaSpawners.prReadiness): what
// a head waits for before its READY TO MERGE verdict. The reviewer checks
// it expects are the review/<reviewer> checks of the reviewers whose
// patterns match the pull request's files, as the review family picks
// them.
type Readiness struct {
	// CIChecks are each repository's required CI check names (ciChecks).
	CIChecks map[string][]string `json:"ci_checks,omitempty"`
	// CubicRepos are the repositories whose Cubic check counts
	// (cubicRepos); elsewhere a stray one is ignored.
	CubicRepos []string `json:"cubic_repos,omitempty"`
	// SettleSeconds is how long after the last check completed a head
	// waits (settleSeconds), so a review's follow-up can appear first.
	SettleSeconds int `json:"settle_seconds,omitempty"`
	// MissingCheckMinutes is how long a head may lack a CI check before
	// its CHECKS MISSING notice (missingCheckNoticeMinutes; 0: none).
	MissingCheckMinutes int `json:"missing_check_minutes,omitempty"`
	// MergeMethod is the auto-merge's GitHub merge method (mergeMethod):
	// merge, squash or rebase; "" is squash.
	MergeMethod string `json:"merge_method,omitempty"`
	// Notify sends a ready head to ntfy (notify.enabled).
	Notify bool `json:"notify,omitempty"`
}

// MergeMethods are the merge methods GitHub accepts; DefaultMergeMethod
// is auto_merge.DEFAULT_MERGE_METHOD.
var MergeMethods = []string{"merge", "squash", "rebase"}

// DefaultMergeMethod is the merge method of an empty MergeMethod.
const DefaultMergeMethod = "squash"

// Method is the merge method, the default for "".
func (r Readiness) Method() string {
	if r.MergeMethod == "" {
		return DefaultMergeMethod
	}
	return r.MergeMethod
}

// Bot is a review bot whose findings start a PR follow-up.
type Bot struct {
	Login              string `json:"login"`
	FindingsPattern    string `json:"findings_pattern"`
	SuggestionsPattern string `json:"suggestions_pattern,omitempty"`
}

// DefaultBaseRef is a Workspace's ref when its project names none
// (kelos.workspaceRef's default).
const DefaultBaseRef = "main"

// Base is the project's Workspace ref.
func (p Project) Base() string {
	if p.BaseRef == "" {
		return DefaultBaseRef
	}
	return p.BaseRef
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

// WorkspaceProject returns the first project whose runs use the
// Workspace on the repository: the project a Workspace's conflict runs
// count against.
func (c Config) WorkspaceProject(repository, workspace string) (Project, bool) {
	for _, p := range c.Projects {
		if p.Repository == repository && p.Workspace == workspace {
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
