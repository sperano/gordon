package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image/png"
	"testing"
)

// mascotSide is the portrait's width and height in pixels.
const mascotSide = 1024

// hollingsworthSHA256 is the sha256 of sperano/hollingsworth
// assets/avatars/gordon-8-bit.png at 4a50f33, taken from that file and
// not from this package, so replacing the asset and MascotSHA256 together
// still fails.
const hollingsworthSHA256 = "033d2e67386e08e94e74386f8e446b9497186bf72d7b1fd5fe9bfa172b4cb093"

func TestMascotIsTheHollingsworthFile(t *testing.T) {
	sum := sha256.Sum256(Mascot)
	if got := hex.EncodeToString(sum[:]); got != hollingsworthSHA256 {
		t.Errorf("sha256 %s, want %s", got, hollingsworthSHA256)
	}
	if MascotSHA256 != hollingsworthSHA256 {
		t.Errorf("MascotSHA256 %s, want %s", MascotSHA256, hollingsworthSHA256)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(Mascot))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != mascotSide || cfg.Height != mascotSide {
		t.Errorf("mascot %dx%d, want %dx%d", cfg.Width, cfg.Height, mascotSide, mascotSide)
	}
}
