# Device Agent AI command sandbox

The remote Agent enforces the AI workspace mode on the device. A client-side
preview or approval never substitutes for this layer.

- Linux functionally probes `bwrap` first. If it cannot apply the profile, the
  Agent re-executes itself through the hidden Landlock runner. The runner uses
  raw Landlock syscalls, reports full/partial ABI enforcement, installs
  `no_new_privs`, and then execs the requested argv.
- macOS wraps the argv in a Seatbelt profile through `sandbox-exec`.
- Windows re-executes the Agent through its restricted-token runner. A
  deterministic workspace capability SID receives a standing inheritable ACL
  grant; a random private temp directory receives a per-run grant that is
  revoked at exit. The target starts suspended, enters a kill-on-close Job
  Object with resource limits, and only then resumes.
- `danger-full-access` is the only deliberate unconfined path. Missing or
  failing runners return `sandbox_unavailable`; there is no silent passthrough.

The result metadata records `sandbox_backend`, `sandbox_enforcement`, and
`hard_network_isolation`. Landlock and Windows ACL constrain filesystem writes
but do not claim host network isolation. Windows enforcement is marked
`partial` because `WRITE_RESTRICTED` must retain `Everyone` and NTFS hard links
can alias an allowed file outside its path.
