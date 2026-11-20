package profile

// LegacyVersion names the Hollingsworth configuration Defaults mirrors:
// helm/kelos/values-hollingsworth.yaml at sperano/hollingsworth bf18aa5
// (2026-11-20), the Gordon 02 execution profiles included.
const LegacyVersion = "hollingsworth-bf18aa5"

// Runner images, as pinned at LegacyVersion.
const (
	claudeImage   = "ericsperano/kelos-claude-code:0.64.0-20261120-031945-6f37b61"
	codexImage    = "ericsperano/kelos-codex:0.64.0-20261120-031945-6f37b61"
	opencodeImage = "ericsperano/kelos-opencode:0.64.0-20261120-031945-6f37b61"
)

// Profile names.
const (
	ProfileDefault           = "default"
	ProfileImageBuild        = "image-build-submitter"
	ProfilePuckDB            = "puckdb"
	ProfileDiscuss           = "discuss"
	ProfileDiscussDefault    = "discuss-default"
	ProfilePuckDBDiscuss     = "puckdb-discuss"
	ProfileReview            = "review"
	ProfilePuckDBReview      = "puckdb-review"
	ProfileMactest           = "mactest"
	ProfileMactestDiscuss    = "mactest-discuss"
	reviewServiceAccount     = "pr-review"
	effortMedium             = "medium"
	defaultVariant           = "claude-opus"
	sonnetVariant            = "claude-sonnet"
	stackGo, stackRust       = "go-service", "rust"
	stackWeb, stackSwift     = "web", "swift"
	discussServiceAccount    = "kelos-discuss"
	imageBuildServiceAccount = "image-build-submitter"
)

// Defaults returns the configuration that reproduces the legacy
// TaskSpawner routing and the Gordon 02 execution profiles (names,
// ServiceAccounts, capabilities) the Kelos admission policy enforces.
func Defaults() Config {
	return Config{
		Version:        LegacyVersion,
		DefaultVariant: defaultVariant,
		Runners:        runners(),
		Variants:       variants(),
		Profiles:       profiles(),
		Projects:       projects(),
		ReviewProfile:  ProfileReview,
		CommonReviewer: []Reviewer{{Name: "pr-challenger", Variant: defaultVariant, Effort: effortMedium}},
		StackReviewers: stackReviewers(),
		Policy:         policy(),
	}
}

func runners() map[string]Runner {
	claudeEfforts := []string{"low", effortMedium, "high", "xhigh", "max"}
	codexEfforts := []string{"low", effortMedium, "high", "xhigh"}
	return map[string]Runner{
		"claude-code": {Type: "claude-code", Image: claudeImage, CredentialType: "oauth",
			CredentialSecret: "claude-oauth", Efforts: claudeEfforts},
		"codex": {Type: "codex", Image: codexImage, CredentialType: "oauth",
			CredentialSecret: "codex-oauth", Efforts: codexEfforts},
		"opencode-deepseek": {Type: "opencode", Image: opencodeImage, CredentialType: "none",
			Efforts: []string{""}},
		"opencode-llamacpp": {Type: "opencode", Image: opencodeImage, CredentialType: "none",
			Efforts: []string{""}},
	}
}

func variants() map[string]Variant {
	return map[string]Variant{
		defaultVariant: {Runner: "claude-code", Model: "claude-opus-5-5[1m]", Effort: effortMedium},
		sonnetVariant:  {Runner: "claude-code", Model: "claude-sonnet-5", Effort: effortMedium},
		"claude-fable": {Runner: "claude-code", Model: "claude-fable-5-1", Effort: effortMedium},
		"claude-haiku": {Runner: "claude-code", Model: "claude-haiku-5", Effort: effortMedium},
		"gpt-sol":      {Runner: "codex", Model: "gpt-5.6-sol", Effort: effortMedium},
		"gpt-astra":    {Runner: "codex", Model: "gpt-6-astra", Effort: effortMedium},
		"deepseek":     {Runner: "opencode-deepseek", Model: "deepseek/deepseek-v4-pro"},
		"qwen-coder":   {Runner: "opencode-llamacpp", Model: "llamacpp/qwen3-coder"},
	}
}

func profiles() map[string]Profile {
	return map[string]Profile{
		ProfileDefault: {Capabilities: []Capability{GitHubWrite, GitHubReadOnly}},
		ProfileImageBuild: {ServiceAccount: imageBuildServiceAccount,
			Capabilities: []Capability{GitHubWrite, GitHubReadOnly, ImageBuildSubmit, ClusterRead}},
		ProfilePuckDB: {ServiceAccount: imageBuildServiceAccount,
			Capabilities: []Capability{GitHubWrite, GitHubReadOnly, ImageBuildSubmit, ClusterRead, PostgresTest, PuckDBProdReadOnly}},
		ProfileMactest: {Capabilities: []Capability{GitHubWrite, GitHubReadOnly, Mactest}},
		ProfileDiscuss: {ServiceAccount: discussServiceAccount, ReadOnly: true,
			Capabilities: []Capability{GitHubReadOnly, ClusterRead}},
		ProfileDiscussDefault: {ReadOnly: true, Capabilities: []Capability{GitHubReadOnly}},
		ProfilePuckDBDiscuss: {ServiceAccount: discussServiceAccount, ReadOnly: true,
			Capabilities: []Capability{GitHubReadOnly, ClusterRead, PostgresTest, PuckDBProdReadOnly}},
		ProfileMactestDiscuss: {ReadOnly: true, Capabilities: []Capability{GitHubReadOnly, Mactest}},
		// The agent container's token is shadowed: only the supersede init
		// container uses pr-review's Task rights.
		ProfileReview: {ServiceAccount: reviewServiceAccount, Capabilities: []Capability{GitHubWrite, GitHubReadOnly}},
		ProfilePuckDBReview: {ServiceAccount: reviewServiceAccount,
			Capabilities: []Capability{GitHubWrite, GitHubReadOnly, PostgresTest, PuckDBProdReadOnly}},
	}
}

func stackReviewers() map[string]Reviewer {
	return map[string]Reviewer{
		stackGo: {Name: "idiomatic-go", Variant: sonnetVariant, Effort: effortMedium,
			FilePatterns: []string{"**/*.go", "**/go.mod", "**/go.sum"}},
		stackRust: {Name: "idiomatic-rust", Variant: sonnetVariant, Effort: effortMedium,
			FilePatterns: []string{"**/*.rs", "**/Cargo.toml", "**/Cargo.lock"}},
		stackWeb: {Name: "idiomatic-typescript", Variant: sonnetVariant, Effort: effortMedium,
			FilePatterns: []string{"**/*.ts", "**/*.tsx", "**/*.js", "**/*.mjs", "**/*.cjs",
				"**/*.svelte", "**/package.json", "**/package-lock.json", "**/tsconfig*.json"}},
		stackSwift: {Name: "idiomatic-swift", Variant: sonnetVariant, Effort: effortMedium,
			FilePatterns: []string{"**/*.swift", "project.yml"}},
	}
}

func policy() Policy {
	return Policy{
		VikunjaOwner: "eric", GitHubOwner: "sperano", Trigger: "/gordon",
		PullRequestAuthors: []string{"sperano", "gordon-cole-ai[bot]"},
		ReviewSkipAuthors:  []string{"renovate[bot]", "dependabot[bot]"},
		InProgressLabel:    "in progress", DiscussLabel: "discuss", NoAgentLabel: "no-agent",
		ChainLabel: "chain", PROpenLabel: "pr open",
		MergeCloseLabels: []string{"in progress", "pr open", "ready to merge", "round-limit-reached", "action-needed"},
		CloseLabels:      []string{"pr open", "ready to merge", "round-limit-reached", "action-needed"},
		DoingBucket:      "Doing", ReviewBucket: "Review",
		VariantLabelPrefix: "kelos:", DraftLabel: "kelos:follow-up",
		FixSuggestionsLabel: "kelos:fix-suggestions", NoReviewLabel: "kelos:no-review",
		ReviewBots: []Bot{
			{Login: "cubic-dev-ai[bot]", FindingsPattern: `\*\*[1-9][0-9]* issues? found\b`},
			{Login: "gordon-cole-ai[bot]", FindingsPattern: `(?m)^\*\*Review verdict: blocking\*\*`,
				SuggestionsPattern: `(?m)^\*\*Review verdict: suggestions\*\*`},
		},
	}
}
