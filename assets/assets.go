// Package assets embeds Gordon's static assets for the README and the
// application-facing pages of later stages.
//
// gordon-8-bit.png is Hollingsworth's existing Gordon Cole mascot, copied
// unchanged from sperano/hollingsworth assets/avatars/gordon-8-bit.png at
// commit 4a50f33 (2026-11-19), where it is the `gordon` service identity's
// avatar. Hollingsworth's assets/avatars/README.md records that it was
// generated with the built-in imagegen tool from the approved concept
// sheet; Hollingsworth declares no separate licence for it.
package assets

import _ "embed"

// MascotSHA256 is the hex SHA-256 of the copied file, as in Hollingsworth.
const MascotSHA256 = "033d2e67386e08e94e74386f8e446b9497186bf72d7b1fd5fe9bfa172b4cb093"

// Mascot is the 8-bit Gordon Cole portrait, a transparent PNG.
//
//go:embed gordon-8-bit.png
var Mascot []byte
