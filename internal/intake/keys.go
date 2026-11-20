package intake

import (
	"os"
	"strings"

	"github.com/sperano/gordon/internal/auth"
	"github.com/sperano/gordon/internal/event"
)

// FileKeys returns a key source that reads each source's key file at
// every call, as the legacy receivers do, so a rotated Secret takes
// effect without a restart. An unreadable or empty file gives that source
// no key (its deliveries are refused 403) and is reported to onError.
func FileKeys(files map[event.Source]string, onError func(event.Source, error)) func() auth.Keys {
	return func() auth.Keys {
		keys := auth.Keys{}
		for src, path := range files {
			b, err := os.ReadFile(path)
			if err != nil {
				onError(src, err)
				continue
			}
			if k := strings.TrimRight(string(b), "\r\n"); k != "" {
				keys[src] = []byte(k)
			}
		}
		return keys
	}
}
