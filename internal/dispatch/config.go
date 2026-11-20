// Package dispatch builds the Kelos Task a run submits from trusted
// configuration only: the run's resolved profile (internal/profile) picks
// the agent, model, effort, ServiceAccount and Workspace, and this
// package's Config supplies the rest of the legacy TaskSpawner
// taskTemplate (prompt, labels, annotations, agent configs, pod pieces),
// keyed by family, Workspace, profile and variant. A new model, effort or
// variant is one more Config entry, never one more TaskSpawner.
//
// Prompt, label and annotation values, agent config names and every pod
// string containing "{{" are Go text/templates over the run's Data
// (Data's doc comment), which mirrors the Kelos template variables the
// legacy spawners use, so Hollingsworth can render this Config from the
// same templates. A missing variable is an error, never an empty string.
package dispatch

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sperano/gordon/internal/route"
	"github.com/sperano/gordon/internal/run"
)

// Reserved Task metadata keys: only Gordon sets them, a Config entry that
// names one is refused.
const (
	// ProfileLabel and ModeLabel name the execution profile the admission
	// policy checks the Task against (Hollingsworth executionProfiles).
	ProfileLabel = "kelos.spe.quebec/profile"
	ModeLabel    = "kelos.spe.quebec/mode"
	// DiscussLabel marks a discuss Task (the failure reporter, admission).
	DiscussLabel = "vikunja.spe.quebec/discuss"
	// VariantLabel is the run's variant: the per-variant concurrency
	// limit counts the Tasks carrying it, the legacy spawners' included.
	VariantLabel = "vikunja.spe.quebec/variant"
	// ManagedLabel marks a Task Gordon submitted.
	ManagedLabel = "gordon.spe.quebec/managed"
	// InboxAnnotation is the inbox row whose decision recorded this
	// generation of the run: the marker that tells Gordon's own Task
	// from another submitter's of the same name.
	InboxAnnotation = "gordon.spe.quebec/inbox"
	// VersionAnnotation is the routing and dispatch configuration
	// versions the Task was built from.
	VersionAnnotation = "gordon.spe.quebec/config-version"
)

var reservedKeys = []string{ProfileLabel, ModeLabel, DiscussLabel, VariantLabel, ManagedLabel, InboxAnnotation, VersionAnnotation}

// Agent config list entries that expand to a list.
const (
	// AgentConfigsProject is the run's project agentConfigs.
	AgentConfigsProject = "@project"
	// AgentConfigVikunja is the variant's Vikunja MCP AgentConfig
	// (vikunja-<assignee>), nothing for a variant without one.
	AgentConfigVikunja = "@vikunja"
)

// DefaultCommentsMaxBytes caps the task comments a prompt quotes (the
// legacy contextSources maxResponseBytes).
const DefaultCommentsMaxBytes = 256 << 10

// Config is the trusted Task configuration.
type Config struct {
	// Version names this configuration; each Task records it next to the
	// routing configuration's.
	Version string `json:"version"`
	// Namespace is where Tasks are created: route.KelosNamespace, the
	// namespace every submit action names.
	Namespace string `json:"namespace"`
	// Comments fetches the Vikunja task's comments for Context.comments;
	// CommentsMaxBytes caps them (a larger answer is left out, as a
	// failed legacy fetch was).
	Comments         bool `json:"comments"`
	CommentsMaxBytes int  `json:"comments_max_bytes,omitempty"`
	// CommonSecrets are the Secrets every profile may reference (runner
	// credentials, Vikunja and Authentik identities, ...): the admission
	// policy's commonSecrets.
	CommonSecrets []string               `json:"common_secrets"`
	Families      map[run.Family]Family  `json:"families"`
	Workspaces    map[string]Pod         `json:"workspaces,omitempty"`
	Profiles      map[string]ProfileSpec `json:"profiles"`
	Variants      map[string]VariantSpec `json:"variants"`
	Limits        Limits                 `json:"limits"`
}

// Family is one family's taskTemplate.
type Family struct {
	Prompt      string            `json:"prompt"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	// AgentConfigs in order; AgentConfigsProject and AgentConfigVikunja
	// expand, every name gets the variant's AgentConfigSuffix.
	AgentConfigs            []string        `json:"agent_configs,omitempty"`
	TTLSecondsAfterFinished *int32          `json:"ttl_seconds_after_finished,omitempty"`
	PodFailurePolicy        json.RawMessage `json:"pod_failure_policy,omitempty"`
	Pod                     Pod             `json:"pod,omitempty"`
}

// ProfileSpec is what a named execution profile adds to the pod: the
// Secrets it grants (besides CommonSecrets) and its reserved pod labels.
type ProfileSpec struct {
	Secrets []string `json:"secrets,omitempty"`
	Pod     Pod      `json:"pod,omitempty"`
}

// VariantSpec is what a variant adds: its runner's AgentConfig suffix
// (OpenCode copies), its Vikunja MCP AgentConfig and its pod pieces (the
// identity and runner env).
type VariantSpec struct {
	AgentConfigSuffix  string `json:"agent_config_suffix,omitempty"`
	VikunjaAgentConfig string `json:"vikunja_agent_config,omitempty"`
	Pod                Pod    `json:"pod,omitempty"`
}

// Pod is a piece of spec.worker.podOverrides. A Task's pod is its
// family's, then its Workspace's, its profile's and its variant's
// pieces, lists concatenated in that order; the last piece that sets
// resources or affinity wins. Only a profile's piece may set labels.
// Init containers start in order: every piece's ExtraInitContainers
// come first, then every piece's LastInitContainers, so a family's
// pr-opener still starts after a profile's test-postgres, as in the
// legacy spawners.
type Pod struct {
	Labels              map[string]string `json:"labels,omitempty"`
	Resources           json.RawMessage   `json:"resources,omitempty"`
	Affinity            json.RawMessage   `json:"affinity,omitempty"`
	Env                 []json.RawMessage `json:"env,omitempty"`
	ExtraInitContainers []json.RawMessage `json:"extraInitContainers,omitempty"`
	LastInitContainers  []json.RawMessage `json:"lastInitContainers,omitempty"`
	Volumes             []json.RawMessage `json:"volumes,omitempty"`
	VolumeMounts        []json.RawMessage `json:"volumeMounts,omitempty"`
}

// Limits bound the Tasks Gordon lets run at once per variant (the legacy
// spawners' maxConcurrency, without the fan-out): a run past its
// variant's limit stays queued in the outbox until a Task finishes.
type Limits struct {
	// Default applies to a variant not in Variants; <= 0 is unbounded.
	Default  int            `json:"default"`
	Variants map[string]int `json:"variants,omitempty"`
}

// For is the limit of a variant; <= 0 is unbounded.
func (l Limits) For(variant string) int {
	if n, ok := l.Variants[variant]; ok {
		return n
	}
	return l.Default
}

// Validate refuses a Config that could build a Task admission refuses or
// that names a reserved key.
func (c Config) Validate() error {
	if c.Namespace != route.KelosNamespace {
		// Every submit action targets kelos/<route.KelosNamespace>/<name>.
		return fmt.Errorf("dispatch config: namespace %q, want %q", c.Namespace, route.KelosNamespace)
	}
	if c.CommentsMaxBytes < 0 {
		return fmt.Errorf("dispatch config: comments_max_bytes %d is negative", c.CommentsMaxBytes)
	}
	for _, f := range slices.Sorted(maps.Keys(c.Families)) {
		if !slices.Contains(run.Families, f) || (run.Identity{Family: f}).Name() == "" {
			return fmt.Errorf("dispatch config: family %q submits no Task", f)
		}
		fam := c.Families[f]
		if strings.TrimSpace(fam.Prompt) == "" {
			return fmt.Errorf("dispatch config: family %s: prompt is required", f)
		}
		if err := checkKeys(fam.Labels, fam.Annotations); err != nil {
			return fmt.Errorf("dispatch config: family %s: %w", f, err)
		}
		if err := fam.Pod.checkNoLabels(); err != nil {
			return fmt.Errorf("dispatch config: family %s: %w", f, err)
		}
	}
	for name, p := range c.Workspaces {
		if err := p.checkNoLabels(); err != nil {
			return fmt.Errorf("dispatch config: workspace %s: %w", name, err)
		}
	}
	for name, v := range c.Variants {
		if err := v.Pod.checkNoLabels(); err != nil {
			return fmt.Errorf("dispatch config: variant %s: %w", name, err)
		}
	}
	for name, p := range c.Profiles {
		if err := checkKeys(p.Pod.Labels); err != nil {
			return fmt.Errorf("dispatch config: profile %s: %w", name, err)
		}
	}
	return nil
}

func checkKeys(sets ...map[string]string) error {
	for _, m := range sets {
		for k := range m {
			if slices.Contains(reservedKeys, k) {
				return fmt.Errorf("%s is set by Gordon", k)
			}
		}
	}
	return nil
}

// checkNoLabels refuses pod labels outside a profile: reserved pod labels
// grant network access, which only a profile may.
func (p Pod) checkNoLabels() error {
	if len(p.Labels) > 0 {
		return fmt.Errorf("pod labels are granted by profiles only")
	}
	return nil
}

// commentsMax is the comments cap in effect.
func (c Config) commentsMax() int {
	if c.CommentsMaxBytes == 0 {
		return DefaultCommentsMaxBytes
	}
	return c.CommentsMaxBytes
}
