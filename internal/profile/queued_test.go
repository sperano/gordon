package profile

import "testing"

func TestValidateQueued(t *testing.T) {
	c := Defaults()
	if err := c.ValidateQueued(); err != nil {
		t.Fatalf("no queued label: %v", err)
	}
	c.Policy.QueuedLabel = "queued"
	if err := c.ValidateQueued(); err != nil {
		t.Fatalf("queued label: %v", err)
	}
	c.Policy.QueuedLabel = c.Policy.InProgressLabel
	want := `policy queued_label "` + c.Policy.InProgressLabel + `" is already policy in_progress_label`
	if err := c.ValidateQueued(); err == nil || err.Error() != want {
		t.Fatalf("queued label shared with in progress: %v, want %s", err, want)
	}
}

func TestValidateOpsRefusesTheQueuedLabel(t *testing.T) {
	c := opsConfig()
	c.Policy.QueuedLabel = opsLabel
	want := `policy ops_label "ops" is already policy queued_label`
	if err := c.ValidateOps(); err == nil || err.Error() != want {
		t.Fatalf("ops label shared with queued: %v, want %s", err, want)
	}
}
