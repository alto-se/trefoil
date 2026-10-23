## Task: write release notes

Optional input: `since`, the tag of the previous release (default: the latest release or tag), and `version`, the name of the upcoming release (default: "Unreleased").

1. Find the previous release with the GitHub tools and note its date.
2. List the pull requests merged into the default branch since then. Read their titles, descriptions and labels; open a PR's diff only when the description doesn't say what changed.
3. Group the changes under these headings, leaving out empty ones: **Breaking changes**, **Features**, **Fixes**, **Performance**, **Documentation**, **Internal**. Breaking changes get a sentence on what users must do.
4. Write one line per change in plain language for users of the project, ending with the PR number and author, e.g. `- Retry failed uploads (#123, @octocat)`. Merge PRs that make up one change; drop pure dependency bumps into a single "Updated dependencies" line.
5. Add a short paragraph at the top summarising the release, and a "New contributors" section for first-time authors.

Save the notes as `/workspace/out/RELEASE_NOTES.md`, save it as an artifact, and reply with the notes themselves.
