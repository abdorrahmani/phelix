# Accessibility Statement

## Our Commitment

Phelix is committed to making software deployment and infrastructure management accessible to as many developers as possible, regardless of their abilities, assistive technology needs, or preferred way of interacting with software.

We believe developer tools should be predictable, understandable, and usable. We aim to reduce unnecessary complexity through clear documentation, consistent command-line behavior, readable output, and interfaces that support different interaction needs.

Accessibility is an ongoing effort. We welcome feedback from users and contributors to help identify barriers and improve the experience over time.

## Scope

This statement covers the Phelix project, including:

- **Command-line interface (CLI):** Commands, terminal output, prompts, errors, and deployment workflows.
- **Web dashboard:** The interface for managing applications, servers, deployments, and related resources.
- **Documentation and project website:** Installation instructions, usage guides, reference documentation, and other project resources.
- **Community and contribution workflows:** GitHub issues, discussions, and contribution documentation.

Accessibility may vary between these components and their respective versions.

## Accessibility Principles

We aim to follow these principles when designing, developing, and maintaining Phelix:

- **Keyboard accessibility:** Where applicable, provide keyboard-operable controls and navigation without requiring a mouse.
- **Clear and consistent output:** Keep CLI messages, errors, status indicators, and instructions readable and understandable.
- **Meaningful structure:** Use semantic HTML, descriptive labels, and appropriate headings in web interfaces and documentation.
- **Visual clarity:** Maintain readable text, sufficient color contrast, and clear distinctions between interface states.
- **Assistive technology compatibility:** Work toward compatibility with screen readers and other assistive technologies.
- **Reduced reliance on color:** Avoid communicating important information through color alone.
- **Understandable errors:** Provide actionable error messages and guidance that help users recover from problems.
- **Documentation quality:** Write clear instructions and provide examples that help users understand and complete tasks.

These principles describe our development goals and do not imply that every criterion has already been implemented or verified.

## Supported Environments

Phelix is primarily designed for developers working in terminal-based and web-based environments.

### Command-Line Interface

The CLI targets supported operating systems and terminal environments documented by the project. Accessibility may depend on the terminal emulator, shell, font configuration, color settings, and assistive technologies being used.

We aim to keep essential command output available as text and avoid making color or visual formatting the only way to understand an operation's result.

### Web Dashboard

The web dashboard is intended for use in modern web browsers supported by the current Phelix release.

We aim to support standard browser interaction patterns, keyboard navigation, readable content, and responsive layouts. Actual accessibility and compatibility depend on the interface, browser, and assistive technology combination.

### Testing Status

We do not currently claim verified conformance with WCAG or comprehensive compatibility with specific screen readers or other assistive technologies unless explicitly documented in project testing results.

Supported environments and verified accessibility coverage will be updated as testing becomes available.

## Known Limitations

Phelix is actively developed, and some accessibility barriers may exist.

Potential areas requiring further evaluation include:

- Keyboard navigation and visible focus indicators across all dashboard workflows.
- Screen reader announcements for dynamic content, deployment status changes, notifications, and validation errors.
- Color contrast and the use of color in status indicators, charts, and other visual elements.
- Accessibility of terminal prompts, progress indicators, tables, and formatted output across different terminal emulators.
- Browser zoom, text resizing, and responsive behavior in complex dashboard views.
- Accessibility coverage across different operating systems, browsers, terminal emulators, and assistive technologies.

This list identifies areas for evaluation and should not be interpreted as a confirmed inventory of defects. Known and verified issues should be documented and tracked as they are identified.

## Reporting Accessibility Barriers

If you encounter an accessibility barrier while using Phelix, we encourage you to report it through our public GitHub repository:

- **Repository:** https://github.com/abdorrahmani/phelix
- **Issue tracker:** https://github.com/abdorrahmani/phelix/issues

When reporting an issue, please include, where possible:

- A description of the accessibility barrier.
- The affected component, command, page, or workflow.
- Your operating system, browser, terminal emulator, or assistive technology and its version, as applicable.
- The steps required to reproduce the issue.
- The expected behavior and the behavior you experienced.

Please avoid including passwords, access tokens, private deployment details, or other sensitive information in public reports.

If you cannot use GitHub's issue tracker because of an accessibility barrier, you may contact the project maintainers through another available project communication channel.

We will review reports as project capacity allows and may request additional information to help reproduce and resolve the issue.

## Contributions

Accessibility improvements are welcome. Contributors can help by:

- Reporting accessibility barriers and suggesting improvements.
- Testing keyboard-only navigation and assistive technology workflows.
- Improving semantic markup, labels, focus management, and color contrast.
- Making CLI output and error messages easier to understand.
- Adding accessibility checks and regression tests where appropriate.

When contributing changes to user-facing interfaces, please consider accessibility alongside functionality, reliability, and usability.

## Maintenance

This statement is maintained by the Phelix project maintainers and may be updated as the project evolves, new accessibility testing is introduced, and reported barriers are addressed.

We are committed to improving accessibility incrementally and being transparent about our progress and limitations.

Last updated: October 2026.
