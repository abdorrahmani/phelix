# Dashboard configuration management

Open an application in the Phelix dashboard and select **Configuration**. The
page reads `phelix.yaml` from the application's managed project directory on its
server. The owning monitor agent must be connected. Viewing the page requires
application viewing permission; creating or saving a file requires permission
to manage application settings.

If the file is missing, choose **Create configuration** to prepare a draft using
the CLI's initialization defaults and the managed application's identity. Review
the draft and select **Save** to create the file. Opening the page never creates
a file.

## Editing

The visual builder groups the options supported by the connected CLI into
categories. Optional groups and repeatable lists can be added or removed. The
builder uses the connected CLI's configuration description, so fields match the
version running on the server. Durations use values such as `10s`, `2m`, and
`24h`; webhook secret settings contain an environment variable name, never the
secret value.

The YAML editor displays the complete document with highlighting, line numbers,
search, formatting, and diagnostics. Both modes edit the same draft. Changing a
visual field updates the YAML, and valid manual YAML changes appear in the
builder. Unknown fields and their nested values remain in the document.
Comments are retained where the YAML editing library can preserve them;
formatting may normalize whitespace and indentation. Invalid YAML stays in the
editor until corrected and cannot be saved.

## Validation and saving

YAML syntax is checked in the browser. The server asks the owning CLI agent to
apply the same configuration validation used by local commands, without running
a build, deployment, hook, or job. Configuration errors identify the relevant
field where the validator provides a location. Validation never saves the file.

Select **Save** to persist a valid changed draft. Each save checks the revision
loaded by the editor, then replaces the file atomically. Identical content does
not rewrite the file. A successful save updates the editor's revision and saved
time. Failed requests retain the draft. Leaving with unsaved changes asks for
confirmation.

If the file changed in another tab or through the CLI, saving reports a
conflict. Compare the latest file with the draft before choosing to reload.
Reloading replaces the draft only after confirmation. A network timeout can
leave the save outcome uncertain; reload the latest metadata or retry the same
content safely before making further changes.

## Applying saved settings

Saving changes the project's file. Builds and rebuilds read it using their
existing configuration resolution rules, including explicit CLI flag overrides.
Saving alone does not restart or deploy the application. Existing commands that
edit configuration share the file lock with dashboard saves.

The document limit is 512 KiB. Remote configuration operations reject symbolic
links and unsafe YAML structures. Security reporting consent remains a local
choice: `security.vulnerability.reporting` can be viewed, but changing its
effective value requires editing the file locally. Configuration management
does not retrieve runtime environment variables or secret-store values.
