// Package kelos is Gordon's view of the Kelos Task API (kelos.dev/v1alpha2
// Tasks): the resource Gordon submits, and a small Kubernetes REST client
// that creates, reads and lists them in one namespace. Pod-level pieces
// (env, volumes, containers, resources, affinity) stay raw JSON: Gordon
// copies them from trusted configuration and never interprets more of
// them than the admission pre-check needs.
package kelos

import "encoding/json"

// API coordinates of a Task.
const (
	Group      = "kelos.dev"
	Version    = "v1alpha2"
	APIVersion = Group + "/" + Version
	KindTask   = "Task"
	resource   = "tasks"
)

// Task phases (TaskStatus.Phase). An empty phase is a Task the controller
// has not picked up yet.
const (
	PhasePending   = "Pending"
	PhaseRunning   = "Running"
	PhaseWaiting   = "Waiting"
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
)

// Task is a Kelos Task.
type Task struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
	Status     *Status  `json:"status,omitempty"`
}

// Metadata is the part of ObjectMeta Gordon sets or reads.
type Metadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	CreationTimestamp string            `json:"creationTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

// Spec is the TaskSpec Gordon submits: always spec.worker (the admission
// policy refuses workerPoolRef and the legacy top-level fields).
type Spec struct {
	Prompt                  string          `json:"prompt"`
	Branch                  string          `json:"branch,omitempty"`
	Worker                  *Worker         `json:"worker,omitempty"`
	TTLSecondsAfterFinished *int32          `json:"ttlSecondsAfterFinished,omitempty"`
	PodFailurePolicy        json.RawMessage `json:"podFailurePolicy,omitempty"`
}

// Worker is spec.worker.
type Worker struct {
	Type            string        `json:"type"`
	Image           string        `json:"image,omitempty"`
	Model           string        `json:"model,omitempty"`
	Effort          string        `json:"effort,omitempty"`
	Credentials     *Credentials  `json:"credentials,omitempty"`
	WorkspaceRef    *Ref          `json:"workspaceRef,omitempty"`
	AgentConfigRefs []Ref         `json:"agentConfigRefs,omitempty"`
	PodOverrides    *PodOverrides `json:"podOverrides,omitempty"`
}

// Credentials is spec.worker.credentials.
type Credentials struct {
	Type      string `json:"type"`
	SecretRef *Ref   `json:"secretRef,omitempty"`
}

// Ref names an object in the Task's namespace.
type Ref struct {
	Name string `json:"name"`
}

// PodOverrides is spec.worker.podOverrides, limited to the fields the
// admission policy allows.
type PodOverrides struct {
	ServiceAccountName  string            `json:"serviceAccountName,omitempty"`
	Labels              map[string]string `json:"labels,omitempty"`
	Resources           json.RawMessage   `json:"resources,omitempty"`
	Affinity            json.RawMessage   `json:"affinity,omitempty"`
	Env                 []json.RawMessage `json:"env,omitempty"`
	ExtraInitContainers []json.RawMessage `json:"extraInitContainers,omitempty"`
	Volumes             []json.RawMessage `json:"volumes,omitempty"`
	VolumeMounts        []json.RawMessage `json:"volumeMounts,omitempty"`
}

// Status is the part of TaskStatus Gordon reads.
type Status struct {
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
}

// Phase is the Task's phase, "" before the controller set one.
func (t Task) Phase() string {
	if t.Status == nil {
		return ""
	}
	return t.Status.Phase
}

// Finished reports a Task that ended (succeeded or failed).
func (t Task) Finished() bool {
	p := t.Phase()
	return p == PhaseSucceeded || p == PhaseFailed
}

// WorkspaceName is the Task's Workspace, "" without one.
func (t Task) WorkspaceName() string {
	if t.Spec.Worker == nil || t.Spec.Worker.WorkspaceRef == nil {
		return ""
	}
	return t.Spec.Worker.WorkspaceRef.Name
}
