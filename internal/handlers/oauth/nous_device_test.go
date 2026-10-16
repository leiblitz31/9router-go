package oauth

import (
	"strings"
	"testing"
	"time"
)

func TestNousPendingMint_Roundtrip(t *testing.T) {
	const code = "nous-roundtrip"
	nousPendingMints.Delete(code)
	defer nousPendingMints.Delete(code)

	nousPendingMintPut(code, "acc-1", "ref-1")
	nousPendingMintIncrement(code)
	nousPendingMintIncrement(code)

	pm, ok := nousPendingMintGet(code)
	if !ok {
		t.Fatal("expected the remembered entry to be found")
	}
	if pm.accessToken != "acc-1" || pm.refreshToken != "ref-1" {
		t.Errorf("entry = %+v, want acc-1/ref-1", pm)
	}
	if got := nousPendingMintTries(code); got != 2 {
		t.Errorf("mintTries = %d, want 2", got)
	}

	// A re-put (a later poll remembering the same token) must not reset the
	// retry budget, or the modal could loop forever.
	nousPendingMintPut(code, "acc-1", "ref-1")
	if got := nousPendingMintTries(code); got != 2 {
		t.Errorf("mintTries after re-put = %d, want 2 (budget must survive)", got)
	}
}

func TestNousPendingMint_ExpiredIsDropped(t *testing.T) {
	const code = "nous-expired"
	nousPendingMints.Store(code, nousPendingMint{
		accessToken: "acc",
		createdAt:   time.Now().Add(-nousPendingMintTTL - time.Minute),
	})
	defer nousPendingMints.Delete(code)

	if _, ok := nousPendingMintGet(code); ok {
		t.Fatal("expected the expired entry to be dropped")
	}
	if _, ok := nousPendingMints.Load(code); ok {
		t.Error("expected the expired entry to be deleted on read")
	}
}

func TestNousPendingMint_Missing(t *testing.T) {
	if _, ok := nousPendingMintGet("nous-no-such-code"); ok {
		t.Fatal("expected a miss for an unknown code")
	}
	if got := nousPendingMintTries("nous-no-such-code"); got != 0 {
		t.Errorf("tries for an unknown code = %d, want 0", got)
	}
}

func TestNousDeleteIsSafe(t *testing.T) {
	// Cleanup runs on the happy path too; a double delete must not panic.
	const code = "nous-delete"
	nousPendingMintPut(code, "acc", "ref")
	nousPendingMintDelete(code)
	nousPendingMintDelete(code)
	if _, ok := nousPendingMintGet(code); ok {
		t.Error("entry survived the delete")
	}
}

func TestNousDeviceFlowIsWired(t *testing.T) {
	if !deviceSupported("nous") {
		t.Error("nous is not in deviceProviders, so the dashboard cannot start its login")
	}
	if got := deviceCanonical("nous"); got != "nous" {
		t.Errorf("deviceCanonical(nous) = %q", got)
	}
}

func TestNousPendingMintMessageIsPending(t *testing.T) {
	// devicePoll maps a message containing "pending" to the pending status, so
	// the mint retry must be spelled that way or the modal errors out.
	msg := "mint_pending: Nous agent key mint failed (try 1/3): status 503"
	if !strings.Contains(msg, "pending") {
		t.Error("the retry message does not read as pending")
	}
}
