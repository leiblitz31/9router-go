### test(translator): pin that keyword-named tool parameters survive the Gemini sanitizer

The Gemini schema sanitizer deletes keywords Google's protobuf has no field for —
`title`, `default`, `const`, `x-*` and the rest of the unsupported list. That is
only correct on a **schema node**: a `properties` value is a dictionary keyed by
parameter name, so a tool parameter called `title` is not a keyword, and a
parameter called `properties` is not the properties container.

Upstream hit this twice. `decolua/9router#4620`: `ensureObjectType` recursed into
the properties map, so a parameter named `properties` made the map itself look like
a `type:"object"` schema and Vertex rejected the request. `decolua/9router#4831`:
`removeUnsupportedKeywords` and `convertConstToEnum` walk `Object.values`, which
deletes keyword-named parameters and rewrites a parameter named `const` into an
enum.

This port is already correct on both counts — `cleanGeminiSchema` recurses into
the **values** of `properties`, never the map itself, so neither failure mode can
reach a parameter name. That is the same rule `stripPatterns` documents
("Properties is special-cased because its keys are arbitrary property names"). No
production code change was needed; what was missing was anything holding the
invariant in place.

**Verified.** `internal/translator/schema_property_names_test.go` runs thirteen
keyword-named parameters through `CleanParametersSchema` and asserts each survives
with its schema and its `required` entry; checks that a parameter named
`properties` is not treated as the properties map in five shapes (flat, beside
siblings, two levels down, inside an array's `items`, in a union branch); and
checks that a parameter named `const` is not converted to an enum. These pass
against the current implementation — they are a guard, not a fix, so a future
"simplify the recursion" refactor fails here instead of at Google with a 400.
