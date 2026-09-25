# Contributing to Phelix

Thanks for your interest in improving Phelix. Contributions to the CLI are
welcome — bug fixes, new features, docs, tests, and platform support all help.

Please read this document before opening a pull request. It covers the
licensing model, how to certify your work, and the project's conventions.

---

## 1. Licensing model — what this means for contributors

Phelix is **source-available**, not classic open source. The CLI is licensed
under the **Functional Source License (FSL-1.1-ALv2)** — see [`LICENSE`](./LICENSE).
In short: you may use, study, modify, and redistribute the code for almost any
purpose **except a Competing Use**, and each released version converts to
Apache 2.0 two years after its release.

A few consequences for contributions:

- **In scope:** anything that makes the Phelix CLI better — features, fixes,
  performance, docs, tests, new platform/toolchain support. There is **no
  limit** on the kinds of CLI features you can add.
- **Out of scope (and not a valid contribution):** work whose purpose is to
  build or wire the CLI up to a **competing dashboard/backend** or a competing
  hosted service. That is a Competing Use under the license, not a
  contribution. Repointing the CLI at your own personal/self-hosted backend for
  your **own** use is fine; shipping that as a rival product/service is not.
- Your contribution is accepted **into Phelix under the same FSL license**, and
  you agree to the terms in sections 5 and 6 below so the project can keep
  maintaining and (eventually) open-sourcing the whole codebase cleanly.

If you are unsure whether an idea is in scope, **open an issue first** and ask —
we would rather talk early than decline a finished PR.

## 2. Before you start

- Search existing issues and discussions first.
- For anything non-trivial, open an issue describing the problem and your
  proposed approach before writing code.
- Keep pull requests focused; one logical change per PR is much easier to review.

## 3. Development setup

Phelix is a single Go binary. You need **Go 1.27+** (see `go.mod`). There is no
Makefile and no CI config — `go build`, `go test`, and `gofmt` are the gates.

```bash
# build
go build -o phelix
./phelix version

# tests
go test ./...                 # all tests
go test -short ./...          # skips the one process-spawning test (internal/app)

# formatting gate (must be clean) + vet
gofmt -l cmd internal config main.go
go vet ./...
```

Deeper architecture and development notes live in [`CLAUDE.md`](./CLAUDE.md) and
under [`docs/`](./docs/) (`docs/architecture`, `docs/development`,
`docs/error-architecture.md`). Please skim the relevant one before large changes
— Phelix has some deliberate invariants (two-phase version promotion, fail-safe
deploys, single error boundary) that reviewers will check against.

## 4. Commit and PR conventions

- **Conventional commits**, with an optional leading emoji:
  `✨ feat(scope): …`, `🐛 fix(scope): …`, `📝 docs(scope): …`.
- Run `gofmt` and `go vet` before pushing; keep `gofmt -l` output empty.
- Add or update tests for behavior changes. Follow the existing seam-injection
  test style (see the "Testing conventions" section of `CLAUDE.md`).
- Describe **what** changed and **why** in the PR body; link the issue.

## 5. Certify your work — Developer Certificate of Origin (DCO)

Every commit must be **signed off** to certify you have the right to submit it
under the project's license. This is the [Developer Certificate of Origin
1.1](https://developercertificate.org/). Add the sign-off automatically with:

```bash
git commit -s -m "✨ feat(deploy): add ..."
```

This appends a line like `Signed-off-by: Your Name <you@example.com>` to the
commit message. By signing off you certify the following:

```
Developer Certificate of Origin
Version 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same license (unless I am permitted to submit
    under a different license), as indicated in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

## 6. Contributor License Agreement (CLA)

Because Phelix is maintained by Mohammad Abdorrahmani under a source-available license that
converts to Apache 2.0 over time, we need contributors to grant Mohammad Abdorrahmani enough
rights to keep the whole codebase under one clean, consistent license (and to
honor the license's future-Apache conversion). By submitting a Contribution,
**you agree to the following.** You keep your copyright — this is a license to
us, not an assignment.

**Definitions.** "You" means the person or entity submitting a Contribution.
"Contribution" means any work of authorship you intentionally submit to the
project (code, docs, or other material).

**1. Copyright license.** You grant Mohammad Abdorrahmani and recipients of software
distributed by Mohammad Abdorrahmani a perpetual, worldwide, non-exclusive, royalty-free,
irrevocable copyright license to reproduce, prepare derivative works of,
publicly display, publicly perform, sublicense, and distribute your Contribution
and such derivative works, **and to relicense the Contribution** as part of
Phelix under the license in [`LICENSE`](./LICENSE), under the Apache License 2.0
(including the license's automatic future-license conversion), and under a
separate commercial license offered by Mohammad Abdorrahmani.

**2. Patent license.** You grant Mohammad Abdorrahmani and recipients a perpetual, worldwide,
non-exclusive, royalty-free, irrevocable (except as stated below) patent license
to make, use, sell, offer to sell, import, and otherwise transfer your
Contribution, covering only those patent claims you can license that are
necessarily infringed by your Contribution alone or by its combination with the
project. If any entity institutes patent litigation alleging the Contribution or
the project infringes a patent, the patent licenses you granted for that
Contribution terminate.

**3. You represent that:** each Contribution is your original creation (or you
have the necessary rights to submit it), you are legally entitled to grant the
above licenses, and — if your employer has rights to work you create — that you
have permission to contribute or your employer has waived such rights. You will
flag any third-party material or license constraints in your Contribution.

**4. No obligation.** Mohammad Abdorrahmani is not obligated to use, merge, or ship any
Contribution. Contributions are provided **"AS IS"**, without warranty of any
kind.

## 7. How agreement is recorded

- **Signing off your commits** (`git commit -s`, section 5) is required on every
  commit and certifies origin under the DCO.
- **Opening a pull request** to this repository indicates that you have read and
  agree to the CLA in section 6 for the Contributions in that PR. For your first
  PR, please add a comment stating: *"I have read the CONTRIBUTING guide and I
  agree to the Phelix CLA."* (If a CLA-assistant bot is enabled on the repo,
  follow its prompt instead.)
- Contributing on behalf of a **company**? Have someone authorized to bind the
  company confirm agreement in the PR, or contact us to arrange an entity CLA.

## 8. Questions

Open a discussion or issue, or reach the maintainers via
https://phelix.anophel.com. Thanks for helping make Phelix better.

