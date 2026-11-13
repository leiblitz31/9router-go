package executor

import (
	"os"
	"testing"

	"9router/proxy/internal/providers"
)

// TestMain keeps the package's unit run off the network.
//
// Two call paths in this package resolve a host at request time and would
// otherwise dial the real internet from a test process:
//
//   - ForwardFreePi resolves the client version before every request, and the
//     resolver refreshes its cached snapshot when the TTL has lapsed. In a test
//     process that cache is always cold, so every ForwardFreePi call dialled
//     api.freepi.ai. The resolver keeps the compiled-in floor when a refresh
//     fails, so a closed local port gives it a deterministic, instant failure.
//   - ForwardQoder reads model_config from the COSY-signed model list, and the
//     cache is cold for every test that does not seed it. The unknown-model
//     path therefore force-refreshes against api3.qoder.sh and used to sit on
//     the egress proxy until the 30s catalogue timeout fired — a real request,
//     from a unit test, to answer a question the fixture already knew.
//
// Pointing both at a closed local port makes "catalogue unavailable" instant
// and offline; the assertions that matter (which model the error names, which
// URL the signature covers) are unchanged.
func TestMain(m *testing.M) {
	closedPort := "http://127.0.0.1:1"

	restoreVersion := providers.SetFreePiClientVersionEndpointForTesting(closedPort + "/client-version")
	providers.ResetFreePiClientVersionForTesting()

	prevModelListURLFor := qoderModelListURLFor
	qoderModelListURLFor = func(QoderRegion, string) string { return closedPort + "/algo/api/v2/model/list" }

	code := m.Run()

	qoderModelListURLFor = prevModelListURLFor
	restoreVersion()
	providers.ResetFreePiClientVersionForTesting()
	os.Exit(code)
}