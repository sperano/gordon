package dispatch

import (
	"encoding/json"
	"strconv"

	"github.com/sperano/gordon/internal/kelos"
)

// applyCoreLabel overrides only the worker's CPU request. The rest of the
// composed resources stay intact, and an unconfigured label stays inert.
func (b Builder) applyCoreLabel(in Input, pod *kelos.PodOverrides) (*kelos.PodOverrides, error) {
	cores, ok, err := b.selectedCores(in)
	if err != nil || !ok {
		return pod, err
	}
	if pod == nil {
		pod = &kelos.PodOverrides{}
	}
	pod.Resources, err = resourcesWithCores(pod.Resources, cores)
	return pod, err
}

func (b Builder) selectedCores(in Input) (int, bool, error) {
	selected := 0
	for _, label := range sourceLabels(in) {
		cores, ok := b.Config.CoreLabels[label]
		if !ok {
			continue
		}
		if selected != 0 && selected != cores {
			return 0, false, refuse(CodeCoreLabelConflict, "labels select both %d and %d cores", selected, cores)
		}
		selected = cores
	}
	return selected, selected != 0, nil
}

func sourceLabels(in Input) []string {
	switch {
	case in.Event.Vikunja != nil:
		if in.State.Task != nil {
			return in.State.Task.Labels
		}
		return in.Event.Vikunja.Task.Labels
	case in.Event.GitHub != nil:
		if in.State.PullRequest != nil {
			return in.State.PullRequest.Labels
		}
		return in.Event.GitHub.PullRequest.Labels
	default:
		return nil
	}
}

func resourcesWithCores(raw json.RawMessage, cores int) (json.RawMessage, error) {
	resources := map[string]json.RawMessage{}
	if raw != nil {
		if err := json.Unmarshal(raw, &resources); err != nil || resources == nil {
			return nil, refuse(CodePod, "resources: want an object")
		}
	}
	requests := map[string]json.RawMessage{}
	if rawRequests, ok := resources["requests"]; ok {
		if err := json.Unmarshal(rawRequests, &requests); err != nil || requests == nil {
			return nil, refuse(CodePod, "resources.requests: want an object")
		}
	}
	cpu, _ := json.Marshal(strconv.Itoa(cores))
	requests["cpu"] = cpu
	encoded, err := json.Marshal(requests)
	if err != nil {
		return nil, refuse(CodePod, "resources.requests: %v", err)
	}
	resources["requests"] = encoded
	encoded, err = json.Marshal(resources)
	if err != nil {
		return nil, refuse(CodePod, "resources: %v", err)
	}
	return encoded, nil
}
