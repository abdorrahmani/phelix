# Licensing & Distribution

This page explains, in plain language, how Phelix is licensed and what you can
and cannot do with it. The binding terms are in [`LICENSE`](../LICENSE) and
[`TRADEMARK.md`](../TRADEMARK.md); this is a friendly summary, not a substitute.

## The short version

Phelix is made of three parts, licensed differently on purpose:

| Part | What it is | How it's licensed |
|---|---|---|
| **Phelix CLI** | The single binary in this repo | **Source-available** under FSL-1.1-ALv2 (converts to Apache 2.0 after 2 years) |
| **Dashboard backend & frontend** | The service at `phelix.anophel.com`, including paid features | **Proprietary / closed source** — not in this repo |
| **"Phelix" name & logo** | The brand | Trademark of Mohammad Abdorrahmani — see `TRADEMARK.md` |

The CLI is fully functional **offline and for free** — build, run, deploy, and
roll back need no account and no network. Logging in only adds optional
dashboard sync.

## What the FSL lets you do

The Functional Source License grants you the right to **use, study, modify, and
redistribute** the CLI for **any purpose except a "Competing Use."** Two years
after each version is released, that version becomes available under the
permissive Apache License 2.0.

Quick guide:

| Can I… | Answer |
|---|---|
| Read, study, and learn from the source? | ✅ Yes |
| Build it myself and run it for my own apps? | ✅ Yes |
| Modify it and self-host for my own use? | ✅ Yes |
| Point the CLI at my **own** self-hosted backend for my own use? | ✅ Yes |
| Fix bugs / add features and contribute them back? | ✅ Yes, please — see `CONTRIBUTING.md` |
| Fork it publicly (to propose changes)? | ✅ Yes (rename any distributed build — see `TRADEMARK.md`) |
| Use it in paid professional services I provide to a client running Phelix? | ✅ Yes |
| Rebrand it and sell it as a competing product? | ❌ No — Competing Use |
| Offer a hosted "Phelix-compatible" dashboard/service to others? | ❌ No — Competing Use |
| Call my fork "Phelix" or use the logo? | ❌ No — trademark |
| Use a 2-year-old version under Apache 2.0? | ✅ Yes — the future-license grant |

"**Competing Use**" is defined in the license as making the software available
to others in a commercial product or service that substitutes for Phelix (or
another product from the same maintainer), or that offers substantially similar functionality.

## Why source-available and not "open source"?

Classic open source (OSI) licenses cannot restrict competing or commercial use —
so they cannot express "free to use and contribute, but not to resell against
us." The FSL can, while still giving you almost all the freedoms of open source
and converting fully to Apache 2.0 over time. We therefore describe Phelix as
**source-available / Fair Source**, not "open source," to be accurate. See the
[FSL FAQ](https://fsl.software/) and the [Fair Source](https://fair.io/)
initiative for background.

## Relationship to the paid dashboard

The parts of Phelix that are paid live in the **dashboard backend**, which is
closed source and not in this repository. Open-sourcing the CLI does not give
away those paid features — the CLI is a client. Running your own backend for
your own apps is fine; offering one as a competing service to others is a
Competing Use.

## See also

- [`LICENSE`](../LICENSE) — the full FSL-1.1-ALv2 text (binding).
- [`TRADEMARK.md`](../TRADEMARK.md) — name and logo policy.
- [`CONTRIBUTING.md`](../CONTRIBUTING.md) — how to contribute (DCO + CLA).
