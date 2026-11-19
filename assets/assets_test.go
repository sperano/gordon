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

func TestMascotIsTheHollingsworthFile(t *testing.T) {
	sum := sha256.Sum256(Mascot)
	if got := hex.EncodeToString(sum[:]); got != MascotSHA256 {
		t.Errorf("sha256 %s, want %s", got, MascotSHA256)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(Mascot))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != mascotSide || cfg.Height != mascotSide {
		t.Errorf("mascot %dx%d, want %dx%d", cfg.Width, cfg.Height, mascotSide, mascotSide)
	}
}
