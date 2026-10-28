### fix(zed): decrypt callbacks from any recent authorize, not just the latest

Every Connect click mints a fresh RSA keypair, but the dialog only holds the
latest verifier — so a callback from an earlier authorize (double-tap on a
laggy tunnel, stale tab, reopened dialog) always failed with "failed to
decrypt Zed access token: crypto/rsa: decryption error", even though the
callback itself was valid.

The authorize handler now keeps recent private keys in a memory-only ring
(50 keys max, 15-minute TTL, never persisted or logged). The exchange tries
the dialog's verifier first, then every ring key, newest first. A callback
encrypted under a key that was never authorized still fails exactly as
before.
