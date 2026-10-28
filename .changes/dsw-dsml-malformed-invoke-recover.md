### fix(deepseek-web): recover a DSML invoke whose closer is missing, and drop markup-only answers instead of leaking them

A malformed turn reached Telegram as a wall of DSML tags. The turn opened an
invoke, closed its command parameter, wrote a stray timeout element, closed
the calls wrapper, and never closed the invoke — so the extractor found no
invoke close and returned nothing, and the answer path then kept the original
text because its strip came back empty.

**Recovery:** `deepSeekDSMLInvokes` now ends an invoke at the calls close when
no invoke close arrives before it. The recovered invoke parses like a closed
one, and elements that are not parameters (the stray timeout) are ignored when
its arguments are built, so nothing undeclared can smuggle into a call. An
invoke with neither close is still dropped, and a normally closed invoke is
unchanged.

**Fail-closed answer:** the tool-turn answer path applied its strip only when
the result was non-empty, which is exactly the malformed case. It now applies
the strip unconditionally: a turn that contained no prose yields an empty
answer instead of the markup. The change is scoped to the deepseek-web
tool-turn call site; the collect path and the transcript helper already
stripped unconditionally, and no other provider calls these helpers.

**Verification:** new table-driven tests for the recovery (missing close at
calls close, no calls close either, normally closed, first closed plus second
recovered) and a tool-turn test driving the real answer path with the
malformed shape, asserting no `DSML` survives and the content is empty.
Full `internal/proxy/executor` suite and `go vet` clean.
