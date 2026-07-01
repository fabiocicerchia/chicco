# Security Policy

## Reporting a vulnerability

Please report security issues privately via GitHub's
[Report a vulnerability](https://github.com/fabiocicerchia/chicco/security/advisories/new)
form (Security → Advisories), **not** a public issue.

Include a description, reproduction steps, and impact. Expect an acknowledgement
within a few days. Once a fix ships, credit is happily given unless you prefer
otherwise.

## Supported versions

Only the latest released version is supported. Fixes ship in a new release.

## Handling secrets

chicco proxies provider API keys. A few things to keep in mind when running it:

- Prefer `${VAR}` references in `chicco.yaml` over literal keys so secrets stay
  out of the file and out of version control.
- chicco binds to `127.0.0.1` by default — do **not** expose it on a public
  interface; it performs no authentication of its own.
- The state file (`chicco-state.json`) holds only per-provider token counters,
  no secrets.

## Release integrity

Release archives ship with an SPDX SBOM and a keyless
[cosign](https://github.com/sigstore/cosign) signature over `checksums.txt`.
See the verification snippet in each release's notes.
