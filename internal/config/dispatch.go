package config

import (
	"time"

	"github.com/sperano/gordon/internal/dispatch"
	"github.com/sperano/gordon/internal/kelos"
)

// Kelos is the worker's Task submission configuration, set when
// GORDON_DISPATCH_CONFIG_FILE is: the trusted Task templates and the
// Kelos API the worker creates, reads and lists Tasks through.
type Kelos struct {
	Dispatch  dispatch.Config
	APIURL    string
	TokenFile string
	CAFile    string
	// BusyDelay is how long a submission deferred by a full variant
	// limit or a held branch waits; ShadowWindow how long a shadow Task
	// waits for the legacy Task of its name.
	BusyDelay    time.Duration
	ShadowWindow time.Duration
}

// Kelos defaults.
const (
	DefaultBusyDelay    = 30 * time.Second
	DefaultShadowWindow = 15 * time.Minute
)

// kelos reads the dispatch configuration and the Kelos API settings; nil
// without GORDON_DISPATCH_CONFIG_FILE.
func (r *reader) kelos() *Kelos {
	var k Kelos
	if !r.jsonFile("GORDON_DISPATCH_CONFIG_FILE", &k.Dispatch) {
		return nil
	}
	if err := k.Dispatch.Validate(); err != nil {
		r.fail("GORDON_DISPATCH_CONFIG_FILE: %v", err)
	}
	k.APIURL = r.baseURL("GORDON_KELOS_API_URL", kelos.InClusterURL)
	k.TokenFile = r.str("GORDON_KELOS_TOKEN_FILE", kelos.InClusterTokenFile)
	k.CAFile = r.str("GORDON_KELOS_CA_FILE", kelos.InClusterCAFile)
	k.BusyDelay = r.duration("GORDON_KELOS_BUSY_DELAY", DefaultBusyDelay)
	k.ShadowWindow = r.duration("GORDON_SHADOW_WINDOW", DefaultShadowWindow)
	return &k
}
