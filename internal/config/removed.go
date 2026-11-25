package config

// RemovedWorkerVars are the worker settings earlier releases read and
// this one ignores: shadow mode (GORDON_SHADOW, GORDON_SHADOW_WINDOW) and
// the ownership table (GORDON_OWNERSHIP_FILE, whose file is not opened).
// Gordon executes every family unconditionally. A deployment may still
// set them, so they are never refused; the worker logs the ones set.
var RemovedWorkerVars = []string{"GORDON_SHADOW", "GORDON_SHADOW_WINDOW", "GORDON_OWNERSHIP_FILE"}

// removed lists the names of vars that are set, in vars' order.
func (r *reader) removed(vars []string) []string {
	var out []string
	for _, name := range vars {
		if r.str(name, "") != "" {
			out = append(out, name)
		}
	}
	return out
}
