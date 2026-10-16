# Repository review guidance

## Review guidelines

### Scope and conduct

- Write review summaries and findings in Simplified Chinese. Keep code identifiers, paths, and command names unchanged.
- Review the current pull request against its actual base branch. Read the complete diff and relevant callers, contracts, and surrounding code. State the commit or branch range reviewed and any unavailable context.
- Judge the result of merging this pull request into its target branch. Inspect other branches only when relevant evidence is explicitly provided; searching descendant branches is not a prerequisite for review.
- During review, provide findings and explanations only. Do not edit code, create commits, push branches, or merge the pull request.

### Operating context

- NimoOS is a personal cloud operating system that runs on a single owner's own hardware. Backend services run as systemd units, mostly as root, and external HTTP traffic reaches them through the Gateway. Use this context, together with the repository's code, as the actual deployment when you apply the evidence rules below.
- The trust model has three tiers. The owner is the first account registered on an uninitialized device and is trusted with full administration of the host. Members are accounts the owner creates; they are trusted only within the rights the owner grants them. Unauthenticated callers, remote clients, and any input that crosses the Gateway are untrusted.
- A finding is a permission-boundary bypass only if an actor gains rights that this trust model does not already give them. An actor who reaches a state they were already entitled to, such as a valid registration-key holder on an uninitialized device ending up with owner-level rights, has not bypassed a boundary.

### Stable blocking threshold

- A blocking finding must be a concrete defect introduced or exposed by this change: a critical supported function becoming unusable, a security or permission boundary being bypassed, data being lost or corrupted, an established interface breaking with consequential impact, or a material violation of an explicit requirement of this change.
- Use P0 for a critical, widespread defect requiring immediate intervention. Use P1 for a high-impact defect with a supported, evidenced execution path that should be fixed before merging. Apply this same threshold in every review round.
- Treat style preferences, optional refactoring, general hardening, speculative failure scenarios, and additional test suggestions as non-blocking P2/P3 unless they demonstrate a defect meeting the blocking threshold. Do not request an architectural redesign solely to eliminate hypothetical risk.

- A scenario whose trigger requires two or more independent failures to occur together is a hardening suggestion (P2) unless this change makes that combination likely or an untrusted actor can cause it. For example, a database write fails and then a local system command fails right after a sibling command succeeded. A single failure under supported operating conditions, such as a transient SQLite lock, a query error, disk exhaustion, or a process crash between two steps, can still be blocking.

### Evidence and actual operating conditions

- For every blocking finding, identify the affected location, concrete trigger, execution path, and material consequence. Support it with a reproducer, a relevant test, or a sufficiently complete code-level trace; do not invent a reproduction or claim checks that were not performed.
- Account for the repository's actual deployment, visibility, caller contracts, permissions, existing protections, and supported inputs. Explain and substantiate any additional assumptions needed for a finding; do not treat an imagined environment as the current one.
- Verify decisive claims about external platform permissions, events, API behavior, or supported configuration against available observed behavior or current official documentation. If the necessary premise cannot be verified, report it as an uncertain, non-blocking question rather than asserting a P0/P1 defect. Uncertainty about an external premise does not invalidate a separately demonstrated defect.
- Inspect available CI failures and distinguish regressions caused by this change from existing base-branch failures, infrastructure failures, and flaky tests. Missing tests alone are not a blocking defect. A PR description claiming validation is not a substitute for inspecting available evidence.

### Follow-up review and conclusion

- On follow-up commits, focus on whether prior blocking findings were fixed and whether the changes introduced regressions. A newly discovered blocker elsewhere in the PR remains reportable only when it meets the same fixed threshold and evidence requirements; do not expand the acceptance criteria between rounds.
- Judge a fix against the trigger and evidence of the original finding. A fix that closes that trigger resolves the finding. Failure modes of the fix itself are new findings: they must meet the blocking threshold on their own and must not be reported as the original finding reopened.
- Blocking findings across rounds must be jointly satisfiable. If an earlier round offered alternative remedies and the author followed one of them, do not block on a property that only the other alternative would have had. If two requirements cannot both be met, for example atomicity across database state and operating-system state, report the trade-off as a non-blocking design question.
- Do not repeat a finding already fixed or disproved by evidence. If reopening it, identify the new evidence and explain why the previous fix or clarification is insufficient.
- Keep findings actionable and proportionate to the pull request. Separate blocking P0/P1 defects from non-blocking P2/P3 suggestions in the review summary. When no finding meets the blocking threshold, explicitly state that no blocking issues were found and the pull request may be merged under these review guidelines.
