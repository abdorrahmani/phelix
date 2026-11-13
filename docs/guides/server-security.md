# Server package security

Use `phelix security server` to inspect operating system packages on the current host, evaluate them against distribution advisories, and review a risk score. Application dependency commands under `phelix security` keep their existing behavior.

## Collect and inspect a snapshot

```bash
phelix security server scan
phelix security server scan --full --json
phelix security server status
phelix security server report --severity high
phelix security server inventory --limit 100 --offset 0
phelix security server inventory --package openssl
phelix security server history --limit 20
phelix security server inventory --scan-id <scan-id> --json
```

Collection reads the package database without refreshing repositories or changing installed packages. `--full` makes the default full-inventory behavior explicit. Interrupt a scan with Ctrl+C. `--timeout 5m` bounds the operation. JSON output uses the standard Phelix result envelope; progress goes to stderr. Failed and cancelled collection attempts remain in history. An inventory alone does not establish that packages are safe.

## Supported systems and prerequisites

| Distribution | Utilities | Inventory | Advisory coverage |
| --- | --- | --- | --- |
| Debian | apt, dpkg-query | dpkg installed records | Debian security tracker release mapping, complete |
| Ubuntu | apt, dpkg-query | dpkg installed records | Ubuntu USN database, complete |
| Alpine Linux | apk | APK installed database | Alpine secdb (main and community), complete |
| Arch Linux | pacman, vercmp | Pacman local database | Arch security tracker, complete |
| Fedora, RHEL, Rocky Linux, AlmaLinux | dnf, rpm | RPM query format | Cached repository updateinfo only, partial |
| openSUSE, Leap, Tumbleweed | zypper, rpm | RPM query format | Cached repository updateinfo only, partial |

Distribution identity comes from os-release. Unsupported distributions and unavailable utilities are reported explicitly. Read access to os-release and the package database is required. Root is normally unnecessary for collection. Run the command in the host environment whose packages you want to inspect; a container sees its own operating system and installed packages.

Full distribution version strings, epochs, architecture, source packages, and multiple installed versions are preserved. dpkg entries that only retain removed-package configuration are excluded. A virtual package is retained as a provider relationship rather than invented as an installed package. Metadata absent from the installed database, such as repositories and pin state, is shown as unknown.

## Vulnerability matching and coverage

A scan evaluates every installed package against the advisory coverage set for its distribution. Sources are fetched over HTTPS with a bounded redirect chain and response size, retried a few times with backoff, and cached under the `host-security/intel-cache` directory of the Phelix data directory. A cached source younger than 6 hours is reused as is; older than 24 hours it is marked stale and the score records the staleness. Enrichment of individual CVEs (OSV.dev, GitHub advisories, NVD, the CISA KEV catalog, EPSS) is capped at 25 CVEs per scan, and the EPSS batch query at 100 CVEs; beyond the caps the missing risk factors stay unknown and a warning is recorded.

Coverage is reported per scan and is never inferred:

- `complete` means the coverage set was loaded, searched, and every finding it produced is confirmed. Debian, Ubuntu, Alpine, and Arch reach this state.
- `partial` means the coverage set could not be fully established (fetch failure, release mismatch, cached-only RPM metadata) or a finding is not confirmed.
- `unknown` means advisories were not evaluated at all, for example on an inventory-only record produced before this feature or after a failed match.

A package the loaded coverage set does not contain is counted in `unknown_packages` and is not clean: its status could not be determined. Debian, Ubuntu, Alpine, and Arch trackers only carry packages with recorded issues, so most packages on a quiet system are legitimately unknown, and a scan of a patched system still reports a large unknown count. RPM-family scans always report zero unknown packages because cached updateinfo is not a complete coverage set by design; their findings are `potential` until the current repository state is verified.

Findings carry per-finding provenance (`sources`) and the scan carries scan-level sources with retrieval time, digest, and staleness. Warnings list source errors verbatim; nothing is fabricated.

## Risk score (risk-v1)

`status` and `history` report a score calculated by the `risk-v1` method from the scan findings:

- Base weights by severity: critical 20, high 10, medium 4, low 1, unknown 4.
- Multipliers within the band: CVSS adds up to 10 percent, EPSS adds up to 50 percent of the predicted exploitation probability, known exploitation doubles the points, a public exploit, network exposure, a critical service, or an important package each add their documented share, and an available fixed version adds 10 percent. Factors the enrichment caps did not populate stay unknown and contribute nothing.
- Each unverified finding adds 2 uncertainty points; stale intelligence adds 10 points.
- Scan age decays the score: 2 points per full day after the first 24 hours, capped at 20.
- The score value is 100 minus the summed penalty, floored at 0. Status is `healthy` only at 90 or above with no findings and fresh intelligence, `high_risk` below 50, and `needs_attention` otherwise.
- Incomplete coverage or a missing scan yields status `unknown` with no value; the score is never invented from unchecked packages.

## Consent and scheduled scans

Reporting consent and the scan cadence are local-only settings; there is no remote toggle and the consent never leaves the host.

```bash
phelix security server config                          # show current consent
phelix security server config --reporting              # consent to scheduled scans
phelix security server config --no-reporting           # revoke consent
phelix security server config --interval 12h           # cadence, minimum 1h
phelix security server config --json
```

The defaults are reporting disabled and a 24 hour cadence. An interval below 1 hour is rejected. Consent is persisted in `host-security/config.json` under the Phelix data directory.

While `phelix monitor` runs and consent is enabled, it schedules host scans on that cadence. The schedule is completion-anchored: the next run is the latest finished scan plus the interval, so restarts and downtime never stack missed runs. A failed scheduled scan retries after 15 minutes, doubling per consecutive failure up to the interval. Scheduled and manual scans share one host scan lock, so they never overlap; a manual scan defers the scheduled round, and its completion re-anchors the cadence. A monitor process that died mid-scan is reclaimed at startup and the scan is never recorded as successful.

## Storage and scripting

Snapshots and immutable scan history are stored in the `host-security` directory under the Phelix data directory. `PHELIX_DATA_DIR` selects that directory. Protect it as private operational data. Package identity snapshots are reused when inventory is unchanged. Queries support `--limit` from 1 to 500 and a nonnegative `--offset`. Keep the scan ID fixed when paging through an inventory.

Successful inventory commands exit 0. Invalid arguments, missing snapshots, failed collection, and cancellation use the standard Phelix error codes documented in the [exit code reference](../reference/exit-codes.md). A successful inventory does not mean vulnerability checks succeeded or that the server is secure.

If collection fails, check that the native utility is installed, the package database is readable, and the distribution version exists in os-release. Phelix does not repair package databases or remove package-manager locks. Output size and package count are bounded; exceeding a bound fails collection instead of presenting truncated data as complete.

## JSON output

`--json` emits the standard machine envelope. The scan result carries provenance and the score; the report result carries the filtered findings with their sources. Fields are omitted when there is nothing to report; empty stays empty.

```json
{
  "schema_version": "1",
  "status": "succeeded",
  "result": {
    "sources": [
      {"name": "debian", "url": "https://security-tracker.debian.org/tracker/data/json", "retrieved_at": "2026-01-01T12:00:00Z", "digest": "...", "stale": false}
    ],
    "score": {
      "version": "risk-v1",
      "value": 89,
      "status": "needs_attention",
      "at": "2026-01-01T12:00:00Z",
      "breakdown": [{"id": "CVE-2024-10001", "points": 11, "priority": "high", "reasons": ["severity: high", "CVSS adds up to 10% within the severity band"]}],
      "reasons": []
    },
    "findings": [
      {
        "id": "0123abcd0123abcd0123abcd0123abcd",
        "package_key": "ssl:amd64@1.0-1",
        "package": "ssl",
        "version": "1.0-1",
        "cve": "CVE-2024-10001",
        "advisory_id": "CVE-2024-10001",
        "ecosystem": "debian:12",
        "affected_range": "distribution advisory status",
        "state": "confirmed",
        "reason": "Debian tracker marks this source package open for bookworm",
        "severity": "high",
        "sources": [],
        "references": [],
        "risk": {"id": "CVE-2024-10001", "severity": "high", "known_exploited": false, "fixed": false, "confidence": "confirmed"}
      }
    ],
    "counts": {"high": 1},
    "unknown_packages": 2,
    "id": "018f0000-0000-7000-8000-000000000000",
    "server_id": "018f0000-0000-7000-8000-000000000001",
    "domain": "server",
    "status": "completed",
    "started_at": "2026-01-01T12:00:00Z",
    "finished_at": "2026-01-01T12:00:01Z",
    "duration_ms": 1000,
    "scanner_version": "dev",
    "method_version": "host-match-v1",
    "snapshot_id": "0123abcd0123abcd0123abcd0123abcd0123abcd0123abcd0123abcd0123abcd",
    "host": {"id": "debian", "name": "Fixture Linux", "version": "12", "kernel": "6.0.0", "architecture": "x86_64", "manager": "apt", "manager_version": "apt 2.6", "hostname": "host", "status": "supported"},
    "package_count": 3,
    "coverage": "complete",
    "warnings": []
  }
}
```

The `report` result is a filtered view: `scan_id`, `status`, `coverage`, `score`, `counts`, `unknown_packages`, `package_count`, `warnings`, `sources`, the matching `findings` with their own provenance, and `total`, `limit`, `offset` for the page. `unknown_packages` and empty arrays are reported as they are; the report never invents findings or factors.

## Limitations

- RPM-family coverage is partial by design: findings come from cached repository updateinfo metadata, are reported as `potential`, and require refreshing the native repository metadata before a scan. Refresh native metadata with the distribution's own tools; Phelix does not run package managers.
- Arch advisory matching requires the `vercmp` utility for version comparisons; the inventory itself does not.
- Enrichment caps apply per scan: 25 CVEs receive OSV, GitHub, and NVD enrichment, and the EPSS query batches at most 100 CVEs. Findings beyond the caps keep unknown risk factors, and a warning names the shortfall.
- A finding is only as current as the coverage set: cached advisory data older than 24 hours marks the score stale rather than pretending the data is fresh.
- An unknown package is not a clean package. Only `complete` coverage with no findings supports a clean statement, and even then only for the evaluated distribution and release.
