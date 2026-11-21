# Supply chain security

Phelix records security evidence alongside each native Go or Rust build. The
evidence describes that build's resolved dependencies, licenses, vulnerability
findings, source, compiler, target platform and artifact digest. You can inspect
stored versions, export standard documents, verify current artifact bytes and
require an application policy before deployment.

## Build evidence

Native builds and rebuilds produce both SPDX 2.3 JSON and CycloneDX 1.6 JSON.
Each document includes the application version, direct and transitive packages,
dependency relationships, ecosystem identifiers, licenses and available
dependency hashes. Go uses the resolved module graph and cross-checks embedded
module metadata when the executable supplies it. Rust uses Cargo metadata with
the selected toolchain, target and feature arguments. Unavailable or incomplete
metadata remains explicit.

Provenance is an in-toto Statement v1 with a SLSA provenance v1 predicate. It
binds the artifact's filename and SHA-256 to the application version, available
repository and full Git revision, source fingerprint, builder, Phelix version,
compiler, target and SBOM digests. Build arguments retain flag names only;
all argument values and environment values are omitted. Missing source or
compiler information is not fabricated.
This document format does not establish a SLSA assurance level or make the local
builder hermetic.

The artifact is hashed immediately after compilation and again before version
persistence and launch. Source and dependency file changes detected during the
build reject its evidence. Build documents are immutable once a version is
recorded; later scans and verification update the associated findings without
replacing how the artifact was built.

## Commands

All commands accept an optional registered application name or ID. Omitting it
selects the project in the current directory, which must contain `phelix.yaml`.
Registered historical artifact inspection does not need the current project's
YAML or a fresh dependency resolution.

| Command | Result |
| --- | --- |
| `phelix security supply-chain list myapp` | A page of stored artifact reports |
| `phelix security supply-chain show myapp` | The selected artifact's full report |
| `phelix security supply-chain sbom myapp` | CycloneDX JSON, or SPDX with `--format` |
| `phelix security supply-chain provenance myapp` | SLSA provenance JSON |
| `phelix security supply-chain inventory myapp` | The stored dependency graph |
| `phelix security supply-chain licenses myapp` | Dependency license findings |
| `phelix security supply-chain scan myapp` | A new OSV scan of the stored inventory |
| `phelix security supply-chain verify myapp` | Recalculated integrity, document, signature and policy checks |
| `phelix security supply-chain sign myapp` | Cosign signatures for the artifact, both SBOMs and provenance, followed by verification |
| `phelix security supply-chain policy myapp` | The current application policy, or an explicitly requested update |

Reports, inventory, licenses, policies and operation results are JSON by default.
SBOM and provenance commands write their JSON document directly to stdout.
There is no separate `--json` flag on `supply-chain` commands. Failed scans,
verification and signing return an error; when a result is available it is also
printed as JSON, so a script must check the exit status.

| Flag | Available on | Behavior |
| --- | --- | --- |
| `--version` | Every artifact command except `list` and `policy` | Positive stored version; omitted or `0` selects the latest version |
| `--digest` | The same artifact commands | Exact lowercase SHA-256 to select an artifact |
| `--build-id` | The same artifact commands | Matrix combination ID to select its independent evidence |
| `--limit` | `list` | Page size from 1 to 100, default 20 |
| `--offset` | `list` | Nonnegative history offset, default 0 |
| `--format` | `sbom` | `spdx-2.3` or `cyclonedx-1.6`, default `cyclonedx-1.6` |
| `--key` | `sign` | Cosign signing key reference; omitted uses keyless signing |
| `--file` | `policy` | JSON policy file; `-` reads stdin |
| `--mode` | `policy` | Set `disabled`, `audit` or `enforce` while preserving other settings |

For example:

```bash
phelix build myapp
phelix security supply-chain list myapp --limit 20 --offset 0
phelix security supply-chain show myapp --version 1
phelix security supply-chain sbom myapp --version 1 --format spdx-2.3 > myapp-v1.spdx.json
phelix security supply-chain sbom myapp --version 1 --format cyclonedx-1.6 > myapp-v1.cdx.json
phelix security supply-chain provenance myapp --version 1 > myapp-v1.provenance.json
phelix security supply-chain inventory myapp --version 1
phelix security supply-chain licenses myapp --version 1
phelix security supply-chain scan myapp --version 1
phelix security supply-chain verify myapp --version 1
```

The `scan`, `verify`, `sign` and policy update commands apply the host's
application security authorization. They use context cancellation and bounded
timeouts. Local artifact commands work without remote reporting or login.

## Matrix builds and history

Each successful native matrix combination has its own report, SBOMs and
provenance. Reports include a `build_id`, such as `go1.27-linux-amd64`, alongside
the version and digest. Different combinations can produce identical bytes;
their evidence is still stored independently.

Use the digest and combination ID returned by `list` when a version has several
artifacts. An ambiguous selection returns an error rather than choosing a
different combination. `--build-id` can also select a unique combination without
supplying its digest:

```bash
phelix security supply-chain show myapp --version 1 --build-id go1.27-linux-amd64
phelix security supply-chain verify myapp --version 1 --build-id go1.27-linux-amd64
```

Matrix artifacts are retained with their version so a later build cannot replace
the bytes referenced by older evidence. History remains inspectable after the
source tree changes. Verification needs the retained artifact; a missing artifact
is an explicit failure. Legacy artifacts without security evidence remain
compatible with audit and disabled policies, while enforce mode requires a
rebuild that records evidence.

## Vulnerabilities and licenses

Artifact rescanning reuses Phelix's OSV scanner against the exact recorded
inventory. It does not resolve today's project dependencies and pretend they
belong to an older binary. Findings include affected package/version, advisory,
severity, fixed version when available, evidence state, source and timestamps.
Scan history identifies newly introduced and previously known findings.

Scan status, coverage and freshness are separate. Not scanned, running, failed,
partial and completed results are not interchangeable with a clean scan.
Unavailable advisory services cannot satisfy a mandatory scan. Enforcement
requires complete, fresh coverage when `require_scan` is set; the current
freshness limit is 24 hours and unknown findings do not satisfy that requirement.
Dependency reachability is not proof that an advisory is exploitable or harmless.

License analysis handles SPDX identifiers and expressions, including alternatives
and exceptions. Known prohibited combinations produce a policy violation.
Missing or unrecognized licenses require manual review instead of being treated
as confirmed violations. An approved alternative in a multiple-license
expression can satisfy the policy, with a review finding when selecting that
alternative matters.

## Policies

The default policy uses audit mode, severity `high`, both SBOMs and provenance,
and warnings for unknown licenses. Mandatory signatures and complete scans are
opt-in. Native builds attempt an artifact scan in audit and enforce modes;
disabled mode skips that automatic scan unless `require_scan` is set.

- `disabled` records evidence without enforcing policy violations.
- `audit` records explanations and violations without blocking on them.
- `enforce` rejects configured policy violations before version acceptance and
  deployment.

Artifact integrity failures always reject verification and launch, including in
audit or disabled mode. A malformed policy fails closed. Enforce mode rejects
legacy artifacts that lack build evidence.

Current audit and enforcement violations remain in the local `phelix.log` with
their application, artifact, rule and reason. Enforced policy errors use
`SECURITY_POLICY_REJECTED`; ordinary deployment telemetry carries a generic
message while full reasons remain in local logs and report decisions.
Security self logs and sensitive results are withheld when remote reporting is
disabled, including entries queued before the disable operation. Ordinary
application and self-log monitoring remain independently controlled.

Create a policy file such as `security-policy.json`:

```json
{
  "mode": "enforce",
  "severity": "high",
  "allowed_licenses": ["MIT", "Apache-2.0", "BSD-3-Clause"],
  "denied_licenses": ["AGPL-3.0-only"],
  "warn_unknown_licenses": true,
  "require_sbom": true,
  "require_provenance": true,
  "require_signature": false,
  "require_scan": true
}
```

Apply or inspect it:

```bash
phelix security supply-chain policy myapp --file security-policy.json
phelix security supply-chain policy myapp
phelix security supply-chain policy myapp --mode audit
```

Policy input rejects unknown fields, trailing data, malformed values and oversized
files. A file updates supplied fields of the current policy; omitted fields
retain their existing values. Inspect the printed policy after applying it.
Allowed and denied lists accept SPDX license identifiers and identifiers
with exceptions; put dependency license expressions in the findings, not in the
policy lists. The combined license list limit is 256 entries.

## Signing and trust

Install Cosign separately and configure the application's trust policy before
signing. Phelix has no default private key or automatic trust root.

For keyless signing, set both `trusted_identity` and `trusted_issuer` to the
identity and OIDC issuer you expect from your signing workflow. For key-based
signing, set `public_key` to a locally installed trusted `.pub` file. When
switching between methods, explicitly clear the previous trust fields in the
policy file. Private
signing credentials belong in Cosign's managed key or credential configuration.
The dashboard cannot change the local public-key reference.

After applying your trust policy, sign a stored version:

```bash
phelix security supply-chain sign myapp --version 1
phelix security supply-chain sign myapp --version 1 --key "$PHELIX_SIGNING_KEY"
phelix security supply-chain verify myapp --version 1
```

The first signing command uses keyless identity. The second requires
`PHELIX_SIGNING_KEY` to contain your configured Cosign key reference. Set
`require_signature` to `true` only after configuring the expected public key or
identity and issuer. Verification rechecks the artifact and every document
bundle against that current trust setting. A valid signature from another
identity is rejected. Public-key verification records the current key's
fingerprint.

Automatic signing at version finalization uses `PHELIX_SIGNING_KEY`, or keyless
signing when `PHELIX_KEYLESS_SIGNING=1`. These settings do not configure trust;
the application policy must still name the expected verifier. Phelix does not
print private signing material or store it in reports.

Signature outcomes distinguish `missing`, `signed`, `verified`, `untrusted`,
`invalid`, `unavailable` and `failed`. A signing failure cannot satisfy mandatory
signatures. Rotate or revoke trust by replacing the configured public key or
expected identity, then verify historical versions again; an old verified status
does not bypass the current policy.

Signing uses Cosign's normal trust and transparency verification. External
identity, transparency and timestamp services must be reachable as required by
your Cosign setup. There is no insecure ignore option in this workflow.

## Persistence and deployment

Reports, policies and content-addressed document bytes live under `supply-chain/`
in the Phelix data directory. Directories use `0700` and files use `0600`.
Individual documents are bounded to 16 MiB and local reports to 32 MiB.
Historical evidence is retained independently of remote reporting activation.

The existing native build, rebuild, service, worker and rollback paths apply
artifact verification. A failed integrity or enforcing policy check cannot
promote the candidate. The established deployment strategies continue to perform
their health checks and routing transitions. Matrix finalization reports policy
or persistence failures instead of a successful release.

This feature covers native Go and Rust artifacts. It does not provide container
image SBOM, provenance or Sigstore verification. Container operations that would
need those mandatory trust checks reject unsupported enforcement rather than
claiming that native evidence verifies an image. See
[Docker runtime](docker-runtime.md).

For server activation, synchronization status and dashboard access, see
[Remote security activation](security.md#remote-security-activation).
