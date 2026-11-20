package kelos_test

import (
	"testing"

	"github.com/sperano/gordon/internal/kelos"
)

func TestTaskPhase(t *testing.T) {
	cases := map[string]bool{"": false, kelos.PhasePending: false, kelos.PhaseRunning: false,
		kelos.PhaseWaiting: false, kelos.PhaseSucceeded: true, kelos.PhaseFailed: true}
	for phase, finished := range cases {
		tk := kelos.Task{}
		if phase != "" {
			tk.Status = &kelos.Status{Phase: phase}
		}
		if tk.Phase() != phase || tk.Finished() != finished {
			t.Errorf("phase %q: Phase %q Finished %v", phase, tk.Phase(), tk.Finished())
		}
	}
	if (kelos.Task{}).WorkspaceName() != "" {
		t.Error("a Task without worker has a Workspace")
	}
}
