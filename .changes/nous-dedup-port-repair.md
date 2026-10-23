---
type: fix
area: providers
summary: Drop duplicate Nous registrations left by converging local and upstream ports
---

The local Nous port and upstream #530 registered the provider twice
(aliases, model URL table, provider definition) and two device-flow
handlers collided; keep the upstream handler with its mint-retry
hardening and a single registration set.
