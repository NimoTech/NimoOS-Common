# Repository review guidance

## Review guidelines

### Scope and conduct

- Write review summaries and findings in Simplified Chinese. Keep code identifiers, paths, and command names unchanged.
- Review the current pull request against its actual base branch. Read the complete diff and relevant callers, contracts, and surrounding code. State the commit or branch range reviewed and any unavailable context.
- Judge the result of merging this pull request into its target branch. Inspect other branches only when relevant evidence is explicitly provided; searching descendant branches is not a prerequisite for review.
- During review, provide findings and explanations only. Do not edit code, create commits, push branches, or merge the pull request.

### Stable blocking threshold

- A blocking finding must be a concrete defect introduced or exposed by this change: a critical supported function becoming unusable, a security or permission boundary being bypassed, data being lost or corrupted, an established interface breaking with consequential impact, or a material violation of an explicit requirement of this change.
- Use P0 for a critical, widespread defect requiring immediate intervention. Use P1 for a high-impact defect with a supported, evidenced execution path that should be fixed before merging. Apply this same threshold in every review round.
- Treat style preferences, optional refactoring, general hardening, speculative failure scenarios, and additional test suggestions as non-blocking P2/P3 unless they demonstrate a defect meeting the blocking threshold. Do not request an architectural redesign solely to eliminate hypothetical risk.

### Evidence and actual operating conditions

- For every blocking finding, identify the affected location, concrete trigger, execution path, and material consequence. Support it with a reproducer, a relevant test, or a sufficiently complete code-level trace; do not invent a reproduction or claim checks that were not performed.
- Account for the repository's actual deployment, visibility, caller contracts, permissions, existing protections, and supported inputs. Explain and substantiate any additional assumptions needed for a finding; do not treat an imagined environment as the current one.
- Verify decisive claims about external platform permissions, events, API behavior, or supported configuration against available observed behavior or current official documentation. If the necessary premise cannot be verified, report it as an uncertain, non-blocking question rather than asserting a P0/P1 defect. Uncertainty about an external premise does not invalidate a separately demonstrated defect.
- Inspect available CI failures and distinguish regressions caused by this change from existing base-branch failures, infrastructure failures, and flaky tests. Missing tests alone are not a blocking defect. A PR description claiming validation is not a substitute for inspecting available evidence.

### Follow-up review and conclusion

- On follow-up commits, focus on whether prior blocking findings were fixed and whether the changes introduced regressions. A newly discovered blocker elsewhere in the PR remains reportable only when it meets the same fixed threshold and evidence requirements; do not expand the acceptance criteria between rounds.
- Do not repeat a finding already fixed or disproved by evidence. If reopening it, identify the new evidence and explain why the previous fix or clarification is insufficient.
- Keep findings actionable and proportionate to the pull request. Separate blocking P0/P1 defects from non-blocking P2/P3 suggestions in the review summary. When no finding meets the blocking threshold, explicitly state that no blocking issues were found and the pull request may be merged under these review guidelines.
