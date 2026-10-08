# Repository review guidance

## Code Review Rules

### Scope and feedback

- Write review summaries and findings in Simplified Chinese. Keep code identifiers, paths, and command names unchanged.
- Read the complete pull request diff and relevant surrounding code before drawing conclusions. State the branch range reviewed and any context you could not access; do not claim checks were performed when they were not.
- During code review, provide findings and explanations only. Do not edit code, create commits, push branches, or merge the pull request.

### Consequential findings

- Prioritize incorrect behavior, security vulnerabilities, data-loss risks, broken interfaces, and missing tests where they affect a real failure. Give a concrete trigger, consequence, and relevant location for each finding. Style preferences and speculative issues are not blocking findings.
- Account for existing caller contracts and repository conventions. A pull request description claiming that a change was tested is not a substitute for inspecting the implementation and available evidence.

### CI and stacked pull requests

- Inspect available CI failures and distinguish failures introduced by this change from existing default-branch failures, infrastructure failures, and flaky tests. Explain the evidence; state uncertainty when the cause cannot be established.
- Review the pull request against its actual base branch. For stacked pull requests, inspect available descendant branches before reporting a blocking finding. If a descendant already fixes the issue, identify that branch and commit and treat it as a note unless merging this pull request alone would leave the default branch broken without an immediate repair from the stack. Do not ask for a fix to be copied backwards solely because it exists in a later branch.
