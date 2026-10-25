package mediaid

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// Key returns a stable opaque identifier for media at a library-relative path.
func Key(rootID, relativePath string) (string, error) {
	if rootID == "" || relativePath == "" {
		return "", errors.New("root id and relative path are required")
	}
	sum := sha256.Sum256([]byte(rootID + "\n" + relativePath))
	return hex.EncodeToString(sum[:])[:32], nil
}
