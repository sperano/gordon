package profile

import "fmt"

// ValidateQueued refuses a queued label another policy label already
// means (an ops label equal to it is ValidateOps's refusal): Gordon removes it from every task whose held run starts or
// stops, so sharing it with, say, the in-progress label would take that
// label off too. An empty label is the queued labelling off.
func (c Config) ValidateQueued() error {
	p := c.Policy
	if p.QueuedLabel == "" {
		return nil
	}
	label := p.QueuedLabel
	p.QueuedLabel = ""
	if name, ok := p.labelNamed(label); ok {
		return fmt.Errorf("policy queued_label %q is already policy %s", label, name)
	}
	return nil
}
