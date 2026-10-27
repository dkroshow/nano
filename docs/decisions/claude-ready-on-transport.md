# Claude readiness follows transport evidence

Claude Code can start its input transport before it emits `system.init`.
Waiting for that init before accepting the first prompt would deadlock: the
init itself may wait for input. Nano therefore distinguishes transport
readiness from confirmed native identity and effective settings.

A resume claims the requested native conversation before async init arrives.
A fresh session may have only a provisional shim handle. Neither is proof that
provider settings were confirmed. Init evidence binds the final identity and
metadata; connection loss or stale replay cannot invent readiness. The shared
[lifecycle](../lifecycle.md) remains the authority for prompt admission.
