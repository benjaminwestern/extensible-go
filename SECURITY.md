# Security

`extensible-go` treats Lua feature packs as trusted local code. The bridge
controls which product seams Lua can replace, but it does not sandbox Lua at the
process or operating-system level.

Do not load untrusted Lua files unless you add an external isolation boundary,
such as a separate process, container, VM, or OS sandbox.

## Current controls

- Unknown registry slots fail validation or reload.
- Invalid replacements for bound slots fail validation or reload.
- Failed reloads keep the previous runtime active.
- Validation runs against isolated staged state and does not mutate the live app.
- Command and event failures return structured diagnostics instead of crashing
  the normal host paths.

## Out of scope

This prototype does not provide CPU, memory, filesystem, network, or wall-clock
limits for Lua code. Add those controls before using the pattern with untrusted
extensions.
