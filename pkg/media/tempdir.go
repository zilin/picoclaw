package media

import (
	"os"
	"path/filepath"
)

const TempDirName = "picoclaw_media"

var customTempDir string

// SetTempDir sets a custom temporary directory overrides default behavior.
func SetTempDir(dir string) {
	customTempDir = dir
}

// TempDir returns the shared temporary directory used for downloaded media.
func TempDir() string {
	if customTempDir != "" {
		return customTempDir
	}
	if dir := os.Getenv("PICOCLAW_MEDIA_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), TempDirName)
}
