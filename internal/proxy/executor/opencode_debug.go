package executor

import (
	"crypto/sha256"
	"encoding/hex"
)

// opencodeDebugHash returns the hex SHA-256 of an outbound request body.
// Only the hash is ever logged: the body itself (system prompt, history,
// tool schemas) must never land in the router log. Comparing hashes across a
// failing agent-originated request and a passing probe tells whether the two
// upstream bodies were byte-identical without ever printing either one.
func opencodeDebugHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
