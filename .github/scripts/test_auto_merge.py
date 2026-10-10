import copy
import http.client
import unittest
import json
import tempfile
import os
from unittest.mock import patch

from auto_merge import AutoMerge, MergeGitHub, changes_policy, eligible, run
from review_gate import BRIDGE, BRIDGE_ID, CODEX, SupersededEvaluation
from test_review_gate import SHA, clean, pr, review, thread


class FakeGitHub:
    repo = "owner/repo"

    def __init__(self):
        self.pr = {**pr(), "node_id": "PR_test", "updated_at": "2026-10-10T04:00:00Z"}
        self.history = [{"state": "success", "creator": {"id": BRIDGE_ID, "login": BRIDGE}}]
        self.e = {"reviews": [], "comments": clean(), "threads": [], "reactions": []}
        self.info = {"id": "PR_test", "state": "OPEN", "isDraft": False,
                     "headRefOid": SHA, "baseRefName": "main", "mergeStateStatus": "CLEAN",
                     "reviewDecision": "APPROVED", "autoMergeRequest": None}
        self.calls, self.reads, self.race, self.superseded = [], 0, None, False
        self.files = [{"filename": "README.md"}]
        self.ci = False
        self.base_tip = "b" * 40
        self.retry_states = []

    finish_followup = MergeGitHub.finish_followup

    def candidate_verified(self, pr):
        pr["_candidate_base"] = self.base_tip
        return True

    def final_snapshot(self, number):
        return {**self.call(f"repos/{self.repo}/pulls/{number}"),
                "base_tip": self.base_tip, "labels_complete": True}

    def verify_candidate_workflow(self, ref):
        self.candidate_ref = ref

    def verify_bridge(self, ref):
        self.bridge_ref = ref

    def refresh_gate(self):
        self.calls.append(("refresh", "POST", None))

    def verify_gate(self, ref):
        self.gate_ref = ref

    def followup(self, pr, state, description):
        self.calls.append(("followup", "POST", {"state": state, "description": description}))

    def recover_followups(self, default):
        pass

    def dispatch_retry(self, default, attempt, numbers, recovery_attempt=0, retry_state=None):
        if retry_state is not None:
            self.retry_states.append(copy.deepcopy(retry_state))
        payload = {"attempt": attempt, "numbers": sorted(numbers)}
        if recovery_attempt:
            payload["recovery_attempt"] = recovery_attempt
        self.calls.append(("retry", "POST", payload))

    def dispatch_workflow(self, workflow, default):
        (self.dispatch_gate if workflow == "review-gate.yml" else self.dispatch_ci)(default)

    def has_ci(self, ref):
        self.ci_ref = ref
        return self.ci

    def dispatch_ci(self, default):
        self.calls.append(("dispatch_ci", "POST", {"ref": default}))

    def pages(self, path, **kwargs):
        if path.endswith("/files"):
            return copy.deepcopy(self.files)
        if path == "repos/owner/repo/pulls?state=open":
            return [copy.deepcopy(self.pr), {**copy.deepcopy(self.pr), "number": 2}]
        raise AssertionError(path)

    def dispatch_gate(self, default):
        self.calls.append(("dispatch", "POST", {"ref": default}))

    def evidence(self, number):
        return copy.deepcopy(self.e)

    def status_history(self, sha):
        return copy.deepcopy(self.history)

    def assert_current_epoch(self, sha, **kwargs):
        if self.superseded:
            raise SupersededEvaluation("new event")

    def call(self, path, method="GET", payload=None):
        if path == "repos/owner/repo" and method == "GET":
            return {"allow_auto_merge": True, "default_branch": "main"}
        if path == "repos/owner/repo/pulls/1" and method == "GET":
            self.reads += 1
            if self.reads == 2 and self.race:
                self.race(self.pr)
            return copy.deepcopy(self.pr)
        self.calls.append((path, method, payload))
        if path.endswith("/merge"):
            return {"merged": True, "sha": "f" * 40}
        if path == "graphql":
            q = payload["query"]
            if q.startswith("query"):
                return {"data": {"repository": {"pullRequest": copy.deepcopy(self.info)}}}
            if "disablePullRequestAutoMerge" in q:
                self.info["autoMergeRequest"] = None
                self.pr["auto_merge"] = None
                self.pr["updated_at"] = "2026-10-10T04:01:00Z"
                return {"data": {"disablePullRequestAutoMerge": {"pullRequest": {"id": "PR_test"}}}}
            return {"data": {"enablePullRequestAutoMerge": {"pullRequest": {
                "id": "PR_test", "autoMergeRequest": {"mergeMethod": "SQUASH"}}}}}
        raise AssertionError((path, method, payload))


class AutoMergeTests(unittest.TestCase):
    def setUp(self):
        self.api = FakeGitHub()
        self.worker = AutoMerge(self.api, "main")

    def mutations(self):
        return [x for x in self.api.calls if x[1] != "GET"
                and not (x[0] == "graphql" and x[2]["query"].startswith("query"))]

    def test_reviewed_clean_head_is_squashed_without_admin(self):
        self.assertEqual(self.worker.sync(self.api.pr), "merged")
        self.assertEqual(self.mutations(), [("repos/owner/repo/pulls/1/merge", "PUT",
                                            {"sha": SHA, "merge_method": "squash"})])

    def test_reviewed_pr_waits_for_remaining_github_requirements(self):
        self.api.info["mergeStateStatus"] = "BLOCKED"
        self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_github_requirements")
        self.assertEqual(self.mutations(), [])

    def test_optional_check_failure_uses_protected_squash_endpoint(self):
        self.api.info["mergeStateStatus"] = "UNSTABLE"
        self.assertEqual(self.worker.sync(self.api.pr), "merged")
        self.assertEqual(self.mutations(), [("repos/owner/repo/pulls/1/merge", "PUT",
                                            {"sha": SHA, "merge_method": "squash"})])

    def test_persistent_auto_merge_is_cancelled(self):
        for method in ["MERGE", "REBASE", "SQUASH"]:
            api = FakeGitHub()
            api.info.update(mergeStateStatus="BLOCKED", autoMergeRequest={"mergeMethod": method})
            self.assertEqual(AutoMerge(api, "main").sync(api.pr), "waiting_for_github_requirements")
            mutations = [c for c in api.calls if c[0] == "graphql" and c[2]["query"].startswith("mutation")]
            self.assertEqual(len(mutations), 1)
            self.assertIn("disablePullRequestAutoMerge", mutations[0][2]["query"])

    def test_changed_head_after_disabling_old_request_is_rechecked(self):
        self.api.info.update(mergeStateStatus="BLOCKED", autoMergeRequest={"mergeMethod": "MERGE"})
        original = self.api.call
        def changed(path, method="GET", payload=None):
            result = original(path, method, payload)
            if path == "graphql" and "disablePullRequestAutoMerge" in payload["query"]:
                self.api.pr["head"]["sha"] = "b" * 40
            return result
        with patch.object(self.api, "call", side_effect=changed):
            self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_valid_review")
        self.assertEqual(len(self.mutations()), 1)

    def test_draft_is_not_registered_or_merged(self):
        self.api.pr["draft"] = True
        self.assertEqual(self.worker.sync(self.api.pr), "not_registered")
        self.assertEqual(self.mutations(), [])

    def test_hold_is_not_registered_or_merged(self):
        self.api.pr["labels"] = [{"name": "HoLd"}]
        self.assertEqual(self.worker.sync(self.api.pr), "not_registered")
        self.assertEqual(self.mutations(), [])

    def test_hold_revokes_existing_auto_merge(self):
        self.api.pr.update(labels=[{"name": "hold"}], auto_merge={"merge_method": "squash"})
        self.assertEqual(self.worker.sync(self.api.pr), "disabled")
        self.assertIn("disablePullRequestAutoMerge", self.mutations()[0][2]["query"])

    def test_draft_revokes_existing_auto_merge(self):
        self.api.pr.update(draft=True, auto_merge={"merge_method": "squash"})
        self.assertEqual(self.worker.sync(self.api.pr), "disabled")

    def test_nondefault_base_is_not_eligible(self):
        self.api.pr["base"]["ref"] = "feature"
        self.assertFalse(eligible(self.api.pr, "main"))

    def test_closed_is_not_eligible(self):
        self.api.pr["state"] = "closed"
        self.assertFalse(eligible(self.api.pr, "main"))

    def test_missing_passing_gate_never_mutates(self):
        self.api.history = []
        self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_review_gate")
        self.assertEqual(self.mutations(), [])

    def test_pending_and_failed_gate_never_mutate(self):
        for state in ["pending", "failure", "error"]:
            self.api.history[0]["state"] = state
            self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_review_gate")
        self.assertEqual(self.mutations(), [])

    def test_spoofed_status_cannot_merge(self):
        for creator in [{"id": 1, "login": BRIDGE}, {"id": BRIDGE_ID, "login": "developer"}]:
            self.api.history[0]["creator"] = creator
            self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_review_gate")
        self.assertEqual(self.mutations(), [])

    def test_new_review_epoch_cannot_reuse_old_success(self):
        self.api.superseded = True
        with self.assertRaises(SupersededEvaluation):
            self.worker.sync(self.api.pr)
        self.assertEqual(self.mutations(), [])

    def test_p1_found_after_passing_gate_cannot_merge(self):
        self.api.e["threads"] = [thread(1)]
        self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_valid_review")
        self.assertEqual(self.mutations(), [])

    def test_re_review_in_progress_cannot_merge(self):
        self.api.e["comments"][0]["body"] = self.api.e["comments"][0]["body"].replace("**Completed**", "**Running**")
        self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_valid_review")
        self.assertEqual(self.mutations(), [])

    def test_stale_sha_cannot_merge(self):
        self.api.pr["head"]["sha"] = "b" * 40
        self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_valid_review")
        self.assertEqual(self.mutations(), [])

    def test_current_changes_requested_cannot_merge(self):
        self.api.e["reviews"] = [review(CODEX, "CHANGES_REQUESTED")]
        self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_valid_review")
        self.assertEqual(self.mutations(), [])

    def test_missing_formal_approval_cannot_merge_clean_head(self):
        self.api.info["reviewDecision"] = "REVIEW_REQUIRED"
        self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_formal_approval")
        self.assertEqual(self.mutations(), [])

    def test_graphql_head_change_is_not_merged(self):
        self.api.info["headRefOid"] = "b" * 40
        self.assertEqual(self.worker.sync(self.api.pr), "changed")
        self.assertEqual(self.mutations(), [])

    def test_metadata_races_do_not_merge(self):
        races = [lambda p: p["head"].update(sha="b" * 40),
                 lambda p: p.update(draft=True), lambda p: p.update(labels=[{"name": "hold"}]),
                 lambda p: p["base"].update(ref="feature"), lambda p: p.update(state="closed"),
                 lambda p: p.update(updated_at="2026-10-10T04:01:00Z")]
        for race in races:
            api = FakeGitHub()
            api.race = race
            self.assertEqual(AutoMerge(api, "main").sync(api.pr), "changed")
            self.assertFalse(any(c[0].endswith("/merge") for c in api.calls))

    def test_preexisting_request_can_merge_only_after_fresh_validation(self):
        self.api.info.update(mergeStateStatus="BLOCKED", autoMergeRequest={"mergeMethod": "SQUASH"})
        self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_github_requirements")
        self.api.info["mergeStateStatus"] = "CLEAN"
        self.assertEqual(self.worker.sync(self.api.pr), "merged")
        self.assertEqual(len(self.mutations()), 2)

    def test_policy_merge_explicitly_dispatches_gate(self):
        for path in ["AGENTS.md", ".github/scripts/review_gate.py", ".github/CODEOWNERS",
                     ".github/workflows/review-gate-events.yml"]:
            api = FakeGitHub()
            api.files = [{"filename": path}]
            self.assertEqual(AutoMerge(api, "main").sync(api.pr), "merged_policy_refreshed")
            self.assertIn(("dispatch", "POST", {"ref": "main"}), api.calls)

    def test_policy_merge_stops_the_remaining_batch(self):
        self.api.files = [{"filename": "AGENTS.md"}]
        with tempfile.NamedTemporaryFile(mode="w+") as f:
            json.dump({"repository": {"default_branch": "main"}}, f); f.flush()
            with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name, "NIMO_GATE_BOOTSTRAP_NUMBERS": "[]"}), \
                 patch("auto_merge.MergeGitHub", return_value=self.api):
                run()
        self.assertEqual(len([c for c in self.mutations() if c[0].endswith("/merge")]), 1)

    def test_failed_policy_dispatch_still_marks_the_batch_obsolete(self):
        self.api.files = [{"filename": "AGENTS.md"}]
        with patch.object(self.api, "dispatch_gate", side_effect=RuntimeError("dispatch failed")):
            with self.assertRaises(RuntimeError):
                self.worker.sync(self.api.pr)
        self.assertTrue(self.worker.policy_merged)

    def test_ci_edits_are_validated_from_candidate_head(self):
        self.api.files = [{"filename": ".github/workflows/ci.yml"}]
        self.api.ci = True
        self.assertEqual(self.worker.sync(self.api.pr), "merged_policy_refreshed")
        self.assertEqual(self.api.ci_ref, SHA)

    def test_older_business_branch_inherits_current_main_ci(self):
        self.assertEqual(self.worker.sync(self.api.pr), "merged")
        self.assertEqual(self.api.ci_ref, "main")

    def test_business_merge_does_not_dispatch_policy_refresh(self):
        self.assertFalse(changes_policy(["main.go", "docs/AGENTS.md"]))
        self.assertEqual(self.worker.sync(self.api.pr), "merged")
        self.assertFalse(any(c[0] == "dispatch" for c in self.api.calls))

    def test_business_merge_preserves_main_ci(self):
        self.api.ci = True
        self.assertEqual(self.worker.sync(self.api.pr), "merged")
        self.assertIn(("dispatch_ci", "POST", {"ref": "main"}), self.api.calls)

    def test_unknown_mergeability_is_retried(self):
        self.api.info["mergeStateStatus"] = "UNKNOWN"
        def calculated(seconds):
            self.api.info["mergeStateStatus"] = "CLEAN"
        with patch("auto_merge.time.sleep", side_effect=calculated):
            self.assertEqual(self.worker.sync(self.api.pr), "merged")

    def test_unknown_mergeability_does_not_register_auto_merge(self):
        self.api.info["mergeStateStatus"] = "UNKNOWN"
        with patch("auto_merge.time.sleep") as sleep:
            self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_github_requirements")
        self.assertEqual(sleep.call_count, 3)
        self.assertEqual(self.mutations(), [])

    def test_p1_arrives_after_last_epoch_read(self):
        calls = 0
        def epoch(sha, **kwargs):
            nonlocal calls
            calls += 1
            if calls == 3:
                self.api.e["threads"] = [thread(1)]
        with patch.object(self.api, "assert_current_epoch", side_effect=epoch):
            self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_valid_review")
        self.assertEqual(self.mutations(), [])

    def test_metadata_change_during_file_read_stops_merge(self):
        def files(path):
            self.api.pr["labels"] = [{"name": "hold"}]
            return [{"filename": "README.md"}]
        with patch.object(self.api, "pages", side_effect=files):
            self.assertEqual(self.worker.sync(self.api.pr), "changed")
        self.assertEqual(self.mutations(), [])

    def test_dispatch_handles_empty_204_response(self):
        api = MergeGitHub.__new__(MergeGitHub)
        api.api, api.repo, api.token = "https://api.github.com", "owner/repo", "test-token"
        from unittest.mock import MagicMock
        response = MagicMock()
        response.__enter__.return_value.status = 204
        with patch("urllib.request.urlopen", return_value=response) as opened:
            api.dispatch_gate("main")
        req = opened.call_args.args[0]
        self.assertEqual(req.full_url, "https://api.github.com/repos/owner/repo/actions/workflows/review-gate.yml/dispatches")
        self.assertEqual(req.data, b'{"ref": "main"}')

    def test_graphql_errors_are_reported(self):
        with patch.object(self.api, "call", return_value={"errors": [{"message": "not allowed"}]}):
            with self.assertRaisesRegex(RuntimeError, "not allowed"):
                self.worker.graphql("mutation", {})

    def test_server_refusal_is_not_reported_as_merged(self):
        original = self.api.call
        def refused(path, method="GET", payload=None):
            return {"merged": False} if path.endswith("/merge") else original(path, method, payload)
        with patch.object(self.api, "call", side_effect=refused):
            with self.assertRaises(RuntimeError):
                self.worker.sync(self.api.pr)

    def test_policy_rename_is_detected_from_previous_filename(self):
        self.api.files = [{"filename": "tools/gate.py", "previous_filename": ".github/scripts/review_gate.py"}]
        self.assertEqual(self.worker.sync(self.api.pr), "merged_policy_refreshed")
        self.assertTrue(self.worker.policy_merged)

    def test_ci_rename_validates_candidate_and_skips_deleted_ci(self):
        self.api.files = [{"filename": ".github/workflows/new.yml", "previous_filename": ".github/workflows/ci.yml"}]
        self.assertEqual(self.worker.sync(self.api.pr), "merged_policy_refreshed")
        self.assertEqual(self.api.ci_ref, SHA)

    def test_gate_edit_validates_candidate_before_merge(self):
        self.api.files = [{"filename": ".github/workflows/review-gate.yml"}]
        with patch.object(self.api, "verify_gate", side_effect=RuntimeError("not dispatchable")):
            with self.assertRaises(RuntimeError):
                self.worker.sync(self.api.pr)
        self.assertFalse(any(c[0].endswith("/merge") for c in self.api.calls))

    def test_behind_uses_normal_server_enforced_merge(self):
        self.api.info["mergeStateStatus"] = "BEHIND"
        self.assertEqual(self.worker.sync(self.api.pr), "merged")

    def test_ci_and_gate_dispatch_failures_are_independent(self):
        self.api.files = [{"filename": "AGENTS.md"}]
        self.api.ci = True
        with patch.object(self.api, "dispatch_ci", side_effect=RuntimeError("temporary API failure")):
            with self.assertRaises(RuntimeError):
                self.worker.sync(self.api.pr)
        self.assertIn(("dispatch", "POST", {"ref": "main"}), self.api.calls)
        intents = [c[2] for c in self.api.calls if c[0] == "followup"]
        self.assertEqual(intents, [{"state": "pending", "description": "v1 ci gate"}, {"state": "pending", "description": "v1 ci"}])
        self.assertTrue(self.worker.policy_merged)

    def test_intent_is_persisted_before_irreversible_merge(self):
        self.api.ci = True
        self.worker.sync(self.api.pr)
        names = [c[0] for c in self.api.calls]
        self.assertLess(names.index("followup"), names.index("repos/owner/repo/pulls/1/merge"))
        self.assertEqual(self.api.calls[-1][2]["state"], "success")

    def test_unknown_schedules_only_bounded_followup(self):
        self.api.info["mergeStateStatus"] = "UNKNOWN"
        for attempt in [0, 3]:
            self.api.calls.clear()
            with tempfile.NamedTemporaryFile(mode="w+") as f:
                json.dump({"repository": {"default_branch": "main"}, "inputs": {"auto_merge_retry": str(attempt)}}, f); f.flush()
                with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name, "NIMO_GATE_BOOTSTRAP_NUMBERS": "[1]"}), \
                     patch("auto_merge.MergeGitHub", return_value=self.api), patch("auto_merge.time.sleep"):
                    if attempt == 3:
                        with self.assertRaisesRegex(RuntimeError, "bounded retries"):
                            run()
                    else:
                        run()
            retries = [c for c in self.api.calls if c[0] == "retry"]
            self.assertEqual(len(retries), int(attempt == 0))
            self.assertFalse(any(c[0].endswith("/merge") for c in self.api.calls))

    def test_reregistered_native_request_is_cancelled_before_exit(self):
        self.api.pr["auto_merge"] = {"merge_method": "squash"}
        with self.assertRaises(RuntimeError):
            self.worker.sync(self.api.pr, cancelled=True)
        self.assertIsNone(self.api.pr["auto_merge"])

    def test_durable_followup_survives_closed_pr_and_requires_trusted_actor(self):
        api = MergeGitHub.__new__(MergeGitHub)
        api.repo = "owner/repo"
        actor = {"databaseId": BRIDGE_ID, "login": BRIDGE}
        intent = {"context": "Nimo Merge Followup", "state": "PENDING", "description": "v1 ci gate", "creator": actor}
        row = {"number": 1, "baseRefName": "main", "headRefOid": SHA, "mergedBy": actor,
               "commits": {"nodes": [{"commit": {"status": {"contexts": [intent]}}}]}}
        page = {"data": {"repository": {"pullRequests": {"nodes": [row], "pageInfo": {"hasNextPage": False}}}}}
        with patch.object(api, "call", return_value=page), patch.object(api, "finish_followup") as finish:
            api.recover_followups("main")
            finish.assert_called_once_with({"number": 1, "head": {"sha": SHA}}, "main", True, True, recovering=True)
            finish.reset_mock()
            row["mergedBy"] = {"login": "developer", "databaseId": 1}
            api.recover_followups("main")
            finish.assert_not_called()

    def test_gate_dispatchability_cannot_be_removed(self):
        api = MergeGitHub.__new__(MergeGitHub)
        api.repo = "owner/repo"
        import urllib.error
        with patch.object(api, "call", side_effect=urllib.error.HTTPError("test", 404, "missing", {}, None)):
            self.assertFalse(api.has_ci(SHA))
            with self.assertRaises(urllib.error.HTTPError):
                api.verify_gate(SHA)


    def test_unknown_merge_response_stops_policy_batch(self):
        self.api.files = [{"filename": "AGENTS.md"}]
        original = self.api.call
        def uncertain(path, method="GET", payload=None):
            if path.endswith("/merge"):
                raise TimeoutError("Server result unknown")
            return original(path, method, payload)
        with patch.object(self.api, "call", side_effect=uncertain):
            with self.assertRaises(TimeoutError):
                self.worker.sync(self.api.pr)
        self.assertTrue(self.worker.policy_merged)

    def test_policy_needs_candidate_tests_before_merge(self):
        self.api.files = [{"filename": ".github/scripts/review_gate.py"}]
        with patch.object(self.api, "candidate_verified", return_value=False):
            self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_candidate_policy_tests")
        self.assertEqual(self.mutations(), [])

    def test_incomplete_pr_file_list_refuses_merge(self):
        self.api.pr["changed_files"] = 3001
        with self.assertRaisesRegex(RuntimeError, "incomplete"):
            self.worker.sync(self.api.pr)
        self.assertEqual(self.mutations(), [])

    def test_auto_request_in_final_snapshot_is_cancelled(self):
        original = self.api.call
        def registered(path, method="GET", payload=None):
            if path.endswith("/pulls/1") and self.api.reads == 3:
                self.api.pr["auto_merge"] = {"merge_method": "squash"}
            return original(path, method, payload)
        with patch.object(self.api, "call", side_effect=registered):
            self.assertEqual(self.worker.sync(self.api.pr), "changed")
        self.assertIsNone(self.api.pr["auto_merge"])
        self.assertFalse(any(c[0].endswith("/merge") for c in self.api.calls))

    def test_recovery_never_dispatches_another_gate(self):
        self.api.files = [{"filename": "AGENTS.md"}]
        with patch.object(self.api, "dispatch_ci", side_effect=RuntimeError("CI unavailable")):
            with self.assertRaises(RuntimeError):
                self.api.finish_followup(self.api.pr, "main", True, True, recovering=True)
        self.assertFalse(any(c[0] == "dispatch" for c in self.api.calls))
        self.assertIn(("refresh", "POST", None), self.api.calls)
        self.assertEqual(self.api.calls[-1][2], {"state": "pending", "description": "v1 ci"})

    def test_yaml_dispatch_must_be_under_on(self):
        api = MergeGitHub.__new__(MergeGitHub);api.repo = "owner/repo"
        import base64
        for body, allowed in [("name: CI\non: [push, pull_request, workflow_dispatch]", True),
                              ("name: CI\non:\n  pull_request:\nenv:\n  workflow_dispatch: yes", False),
                              ('name: CI\n"on":\n    pull_request:\n    workflow_dispatch:', True),
                              ("name: Other CI\non: [pull_request, workflow_dispatch]", False)]:
            with patch.object(api, "call", return_value={"content": base64.b64encode(body.encode()).decode()}):
                if allowed:
                    self.assertTrue(api.has_ci(SHA))
                else:
                    with self.assertRaises(RuntimeError):
                        api.has_ci(SHA)

    def test_candidate_run_requires_trusted_workflow_identity_and_head(self):
        api = MergeGitHub.__new__(MergeGitHub);api.repo = "owner/repo"
        run = {"id": 22, "workflow_id": 7, "event": "pull_request", "path": ".github/workflows/review-policy-tests.yml",
               "display_title": f"Candidate review policy for PR #1 at {SHA}", "status": "completed", "conclusion": "success"}
        job = {"name": "candidate", "status": "completed", "conclusion": "success", "steps": [
            {"name": "Test candidate policy without privileged credentials", "status": "completed", "conclusion": "success"}]}
        def pages(path, **kwargs):
            return [job] if path.endswith("/jobs") else [run]
        def call(path, *args, **kwargs):
            return {"object": {"sha": "b" * 40}, "status": "ahead", "id": 7, "sha": "same-source"}
        with patch.object(api, "call", side_effect=call), patch.object(api, "pages", side_effect=pages):
            self.assertTrue(api.candidate_verified(self.api.pr))
            for key, bad in [("workflow_id", 9), ("event", "pull_request_target"), ("path", ".github/workflows/untrusted.yml"),
                             ("display_title", "Candidate review policy for PR #1 at " + "b" * 40), ("conclusion", "failure")]:
                original = run[key];run[key] = bad
                self.assertFalse(api.candidate_verified(self.api.pr))
                run[key] = original

    def test_bridge_edit_requires_candidate_review_events(self):
        self.api.files = [{"filename": ".github/workflows/review-gate-events.yml"}]
        with patch.object(self.api, "verify_bridge", side_effect=RuntimeError("bridge event missing")):
            with self.assertRaises(RuntimeError):
                self.worker.sync(self.api.pr)
        self.assertFalse(any(c[0].endswith("/merge") for c in self.api.calls))


    def test_recovery_refresh_clears_scope_and_restores_it_on_error(self):
        api = MergeGitHub.__new__(MergeGitHub)
        def refresh():
            self.assertEqual(os.environ["NIMO_GATE_BOOTSTRAP_NUMBERS"], "[]")
            self.assertEqual(os.environ["NIMO_GATE_BOOTSTRAP_HEADS"], "[]")
            self.assertTrue(os.environ["NIMO_GATE_BOOTSTRAP_STARTED_AT"])
            self.assertEqual(os.environ["NIMO_GATE_EPOCH"], "123.1")
            raise RuntimeError("retry")
        with patch.dict(os.environ, {"NIMO_GATE_BOOTSTRAP_NUMBERS": "[1]", "NIMO_GATE_EPOCH": "123.1"}), \
             patch("auto_merge.refresh_review_gate", side_effect=refresh), \
             patch.object(api, "assert_checkout_policy_current"):
            with self.assertRaises(RuntimeError):
                api.refresh_gate()
            self.assertEqual(os.environ["NIMO_GATE_BOOTSTRAP_NUMBERS"], "[1]")

    def test_candidate_workflow_write_permission_is_rejected(self):
        api = MergeGitHub.__new__(MergeGitHub)
        config = {"name": "Review Policy Tests", "run-name": 'Candidate review policy for PR #${{ github.event.pull_request.number }} at ${{ github.event.pull_request.head.sha }}',
                  "on": {"pull_request": {}}, "permissions": {"contents": "write"}, "jobs": {"candidate": {}}}
        with patch.object(api, "workflow", return_value=config):
            with self.assertRaises(RuntimeError):
                api.verify_candidate_workflow(SHA)


    def test_deployed_workflows_parse_and_keep_required_contracts(self):
        from pathlib import Path
        import base64
        root = Path(__file__).resolve().parent
        api = MergeGitHub.__new__(MergeGitHub);api.repo = "owner/repo"
        methods = {"review-gate.yml": api.verify_gate, "review-gate-events.yml": api.verify_bridge,
                   "review-policy-tests.yml": api.verify_candidate_workflow}
        for name, validate in methods.items():
            path = root / name
            if not path.exists():
                path = root.parent / "workflows" / name
            with patch.object(api, "call", return_value={"content": base64.b64encode(path.read_bytes()).decode()}):
                validate(SHA)


    def test_modified_candidate_runner_cannot_attest_itself(self):
        api = MergeGitHub.__new__(MergeGitHub);api.repo = "owner/repo"
        with patch.object(api, "call", side_effect=[{"object": {"sha": "b" * 40}}, {"status": "ahead"}, {"sha": "trusted"}, {"sha": "modified"}]):
            self.assertFalse(api.candidate_verified(self.api.pr))


    def test_global_recovery_initializes_both_previously_owned_heads(self):
        from review_gate import GitHub
        api = MergeGitHub.__new__(MergeGitHub)
        gate = GitHub.__new__(GitHub);gate.repo = "owner/repo"
        owners = {SHA: "old.1", "b" * 40: "old.2"}
        def initialize(sha, state, initialize=False):
            self.assertTrue(initialize)
            self.assertEqual(state, "pending")
            owners[sha] = os.environ["NIMO_GATE_EPOCH"]
        def assert_owner(sha):
            self.assertEqual(owners[sha], os.environ["NIMO_GATE_EPOCH"])
        def refresh():
            for sha in owners:
                gate.check(sha)
        with patch.dict(os.environ, {"NIMO_GATE_EPOCH": "123.1", "NIMO_GATE_BOOTSTRAP_HEADS": json.dumps([SHA]),
                                     "NIMO_GATE_BOOTSTRAP_NUMBERS": "[1]", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_RUN_ID": "123"}), \
             patch.object(gate, "status", side_effect=initialize), patch.object(gate, "assert_current_epoch", side_effect=assert_owner), \
             patch.object(gate, "pages", return_value=[]), patch.object(gate, "call", return_value={"id": 1, "app": {"id": 15368}}), \
             patch("auto_merge.refresh_review_gate", side_effect=refresh), \
             patch.object(api, "assert_checkout_policy_current"):
            api.refresh_gate()
        self.assertEqual(set(owners.values()), {"123.1"})




    def test_skipped_candidate_job_or_test_step_cannot_attest_policy(self):
        api = MergeGitHub.__new__(MergeGitHub);api.repo = "owner/repo"
        run = {"id": 22, "workflow_id": 7, "event": "pull_request", "path": ".github/workflows/review-policy-tests.yml",
               "display_title": f"Candidate review policy for PR #1 at {SHA}", "status": "completed", "conclusion": "success"}
        job = {"name": "candidate", "status": "completed", "conclusion": "success", "steps": [
            {"name": "Test candidate policy without privileged credentials", "status": "completed", "conclusion": "success"}]}
        def pages(path, **kwargs):
            return [job] if path.endswith("/jobs") else [run]
        with patch.object(api, "call", return_value={"object": {"sha": "b" * 40}, "status": "ahead", "id": 7, "sha": "same"}), \
             patch.object(api, "pages", side_effect=pages):
            self.assertTrue(api.candidate_verified(self.api.pr))
            job["conclusion"] = "skipped"
            self.assertFalse(api.candidate_verified(self.api.pr))
            job["conclusion"] = "success"
            job["steps"][0]["conclusion"] = "skipped"
            self.assertFalse(api.candidate_verified(self.api.pr))

    def test_policy_head_must_include_current_default_branch(self):
        api = MergeGitHub.__new__(MergeGitHub);api.repo = "owner/repo"
        for status in ["behind", "diverged"]:
            with patch.object(api, "call", side_effect=[{"object": {"sha": "b" * 40}}, {"status": status}]), \
                 patch.object(api, "pages") as pages:
                self.assertFalse(api.candidate_verified(self.api.pr))
                pages.assert_not_called()

    def test_hold_during_candidate_verification_prevents_merge(self):
        self.api.files = [{"filename": ".github/scripts/review_gate.py"}]
        calls = []
        def verify(pr):
            calls.append(pr)
            if len(calls) == 2:
                self.api.pr["labels"] = [{"name": "hold"}]
            return True
        with patch.object(self.api, "candidate_verified", side_effect=verify):
            self.assertEqual(self.worker.sync(self.api.pr), "changed")
        self.assertFalse(any(c[0].endswith("/merge") for c in self.api.calls))

    def test_new_epoch_during_candidate_verification_prevents_merge(self):
        self.api.files = [{"filename": ".github/scripts/review_gate.py"}]
        calls = []
        def verify(pr):
            calls.append(pr)
            if len(calls) == 2:
                self.api.superseded = True
            return True
        with patch.object(self.api, "candidate_verified", side_effect=verify):
            with self.assertRaises(SupersededEvaluation):
                self.worker.sync(self.api.pr)
        self.assertFalse(any(c[0].endswith("/merge") for c in self.api.calls))

    def test_policy_base_advance_during_verification_prevents_merge(self):
        self.api.files = [{"filename": ".github/scripts/review_gate.py"}]
        with patch.object(self.api, "candidate_verified", side_effect=[True, False]):
            self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_candidate_policy_tests")
        self.assertFalse(any(c[0].endswith("/merge") for c in self.api.calls))


    def test_base_tip_advance_after_successful_policy_proof_prevents_merge(self):
        self.api.files = [{"filename": ".github/scripts/review_gate.py"}]
        original = self.api.candidate_verified
        calls = 0
        def verify(pr):
            nonlocal calls
            calls += 1
            verified = original(pr)
            if calls == 2:
                self.api.base_tip = "c" * 40
            return verified
        with patch.object(self.api, "candidate_verified", side_effect=verify):
            self.assertEqual(self.worker.sync(self.api.pr), "waiting_for_candidate_policy_tests")
        self.assertFalse(any(c[0].endswith("/merge") for c in self.api.calls))

    def test_failed_candidate_proof_still_revokes_new_native_request(self):
        for failed_call in [1, 2]:
            with self.subTest(failed_call=failed_call):
                api = FakeGitHub()
                api.files = [{"filename": ".github/scripts/review_gate.py"}]
                original = api.candidate_verified
                calls = 0
                def verify(pr):
                    nonlocal calls
                    calls += 1
                    if calls == failed_call:
                        api.pr["auto_merge"] = {"merge_method": "squash"}
                        return False
                    return original(pr)
                with patch.object(api, "candidate_verified", side_effect=verify):
                    self.assertEqual(AutoMerge(api, "main").sync(api.pr), "changed")
                self.assertIsNone(api.pr["auto_merge"])
                self.assertFalse(any(c[0].endswith("/merge") for c in api.calls))

    def test_graphql_final_snapshot_uses_live_base_and_rejects_partial_labels(self):
        api = MergeGitHub.__new__(MergeGitHub);api.repo = "owner/repo"
        row = {"id": "PR_test", "state": "OPEN", "isDraft": False,
               "headRefOid": SHA, "baseRefName": "main", "updatedAt": self.api.pr["updated_at"],
               "labels": {"nodes": [], "pageInfo": {"hasNextPage": False}},
               "autoMergeRequest": {"mergeMethod": "SQUASH"}, "baseRef": {"target": {"oid": "c" * 40}}}
        response = {"data": {"repository": {"pullRequest": row}}}
        with patch.object(api, "call", return_value=response) as call:
            snapshot = api.final_snapshot(1)
            self.assertEqual(snapshot["base_tip"], "c" * 40)
            self.assertEqual(snapshot["auto_merge"], {"mergeMethod": "SQUASH"})
            self.assertTrue(eligible(snapshot, "main"))
            row["labels"]["pageInfo"]["hasNextPage"] = True
            partial = api.final_snapshot(1)
            self.assertFalse(eligible(partial, "main"))
            with patch.object(self.api, "final_snapshot", return_value={**partial, "auto_merge": None}):
                self.assertEqual(self.worker.sync(self.api.pr), "changed")
        self.assertFalse(any(c[0].endswith("/merge") for c in self.api.calls))

    def test_lost_merge_response_immediately_runs_confirmed_merge_followups(self):
        self.api.files = [{"filename": "AGENTS.md"}]
        self.api.ci = True
        original = self.api.call
        def lost(path, method="GET", payload=None):
            if path.endswith("/merge"):
                self.api.pr["merged"] = True
                self.api.pr["state"] = "closed"
                raise TimeoutError("Successful server response lost")
            return original(path, method, payload)
        with patch.object(self.api, "call", side_effect=lost):
            self.assertEqual(self.worker.sync(self.api.pr), "merged_policy_refreshed")
        self.assertIn(("dispatch_ci", "POST", {"ref": "main"}), self.api.calls)
        self.assertIn(("dispatch", "POST", {"ref": "main"}), self.api.calls)
        self.assertEqual(self.worker.recovery_needed, set())

    def test_unconfirmed_merge_recovery_dispatch_is_bounded(self):
        for attempt in [0, 3]:
            with self.subTest(attempt=attempt):
                api = FakeGitHub();api.files = [{"filename": "AGENTS.md"}]
                original = api.call
                def uncertain(path, method="GET", payload=None):
                    if path.endswith("/merge"):
                        raise TimeoutError("Server result unavailable")
                    return original(path, method, payload)
                with tempfile.NamedTemporaryFile(mode="w+") as f:
                    json.dump({"repository": {"default_branch": "main"}, "inputs": {"auto_merge_recovery_retry": str(attempt)}}, f);f.flush()
                    with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name, "NIMO_GATE_BOOTSTRAP_NUMBERS": "[1]"}), \
                         patch("auto_merge.MergeGitHub", return_value=api), patch.object(api, "call", side_effect=uncertain), \
                         patch("auto_merge.time.sleep"):
                        with self.assertRaises(RuntimeError):
                            run()
                retries = [c for c in api.calls if c[0] == "retry"]
                self.assertEqual(len(retries), int(attempt == 0))
                if retries:
                    self.assertEqual(retries[0][2], {"attempt": 0, "numbers": [1], "recovery_attempt": 1})

    def test_definitive_merge_rejection_does_not_schedule_ambiguous_recovery(self):
        import urllib.error
        for code in [403, 405, 409]:
            with self.subTest(code=code):
                api = FakeGitHub();api.files = [{"filename": "AGENTS.md"}]
                worker = AutoMerge(api, "main")
                original = api.call
                def refused(path, method="GET", payload=None):
                    if path.endswith("/merge"):
                        raise urllib.error.HTTPError("test", code, "refused", {}, None)
                    return original(path, method, payload)
                with patch.object(api, "call", side_effect=refused):
                    with self.assertRaises(urllib.error.HTTPError):
                        worker.sync(api.pr)
                self.assertEqual(worker.recovery_needed, set())
                self.assertFalse(any(c[0] in {"dispatch", "dispatch_ci"} for c in api.calls))

    def test_policy_refresh_preserves_original_epoch_boundary(self):
        from review_gate import GitHub
        api = MergeGitHub.__new__(MergeGitHub)
        gate = GitHub.__new__(GitHub)
        marker = {"creator": {"id": BRIDGE_ID, "login": BRIDGE},
                  "description": "Review epoch 124.1: new event", "created_at": "2026-10-10T04:01:00Z"}
        def refresh():
            self.assertEqual(os.environ["NIMO_GATE_BOOTSTRAP_STARTED_AT"], "2026-10-10T04:00:00Z")
            gate.assert_current_epoch(SHA, history=[marker], initialize=True)
        with patch.dict(os.environ, {"NIMO_GATE_EPOCH": "123.1", "NIMO_GATE_BOOTSTRAP_STARTED_AT": "2026-10-10T04:00:00Z"}), \
             patch.object(api, "assert_checkout_policy_current"), patch("auto_merge.refresh_review_gate", side_effect=refresh):
            with self.assertRaises(SupersededEvaluation):
                api.refresh_gate()
        self.assertFalse(getattr(api, "recovered_policy", False))

    def test_policy_refresh_allows_business_main_advance_but_stops_policy_change(self):
        api = MergeGitHub.__new__(MergeGitHub);api.repo = "owner/repo"
        checkout, main = "b" * 40, "c" * 40
        github_tree = ["same"]
        def call(path, *args, **kwargs):
            if path == "repos/owner/repo":
                return {"default_branch": "main"}
            if "/git/ref/" in path:
                return {"object": {"sha": main}}
            return {"tree": [{"path": ".github", "type": "tree", "sha": github_tree[0] if path.endswith(main) else "same"},
                             {"path": "AGENTS.md", "type": "blob", "sha": "same-rules"}]}
        with patch("auto_merge.subprocess.check_output", return_value=checkout), patch.object(api, "call", side_effect=call), \
             patch("auto_merge.refresh_review_gate") as refresh:
            api.refresh_gate()
            self.assertTrue(api.recovered_policy)
            api.recovered_policy = False
            github_tree[0] = "new-policy"
            with self.assertRaises(SupersededEvaluation):
                api.refresh_gate()
            self.assertFalse(api.recovered_policy)
            self.assertEqual(refresh.call_count, 1)
            github_tree[0] = "same"
            refresh.side_effect = lambda: github_tree.__setitem__(0, "new-policy")
            with self.assertRaises(SupersededEvaluation):
                api.refresh_gate()
            self.assertFalse(api.recovered_policy)

    def test_successful_global_policy_recovery_removes_old_pr_scope(self):
        api = FakeGitHub()
        api.recovered_policy = True
        original = api.call
        def other_pr(path, method="GET", payload=None):
            if path == "repos/owner/repo/pulls/2" and method == "GET":
                return {**copy.deepcopy(api.pr), "number": 2}
            return original(path, method, payload)
        with tempfile.NamedTemporaryFile(mode="w+") as f:
            json.dump({"repository": {"default_branch": "main"}}, f);f.flush()
            with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name, "NIMO_GATE_BOOTSTRAP_NUMBERS": "[1]"}), \
                 patch("auto_merge.MergeGitHub", return_value=api), patch.object(api, "call", side_effect=other_pr):
                run()
        merged = [c[0] for c in api.calls if c[0].endswith("/merge")]
        self.assertEqual(merged, ["repos/owner/repo/pulls/1/merge", "repos/owner/repo/pulls/2/merge"])


    @staticmethod
    def incomplete_http_response(path, method="GET", payload=None):
        from review_gate import GitHub
        api = GitHub.__new__(GitHub)
        api.api, api.token = "https://api.github.com", "test-token"
        # Exercise the real urllib response -> json.load path, rather than a
        # synthetic transport exception raised directly by the fake client.
        with patch("review_gate.urllib.request.urlopen") as urlopen:
            urlopen.return_value.__enter__.return_value.read.side_effect = \
                http.client.IncompleteRead(b'{"merged": true', 8)
            return GitHub.call(api, path, method, payload)

    def test_incomplete_merge_response_immediately_runs_confirmed_followups(self):
        self.api.files = [{"filename": "AGENTS.md"}]
        self.api.ci = True
        original = self.api.call
        def incomplete(path, method="GET", payload=None):
            if path.endswith("/merge"):
                self.api.pr.update(merged=True, state="closed")
                return self.incomplete_http_response(path, method, payload)
            return original(path, method, payload)
        with patch.object(self.api, "call", side_effect=incomplete):
            self.assertEqual(self.worker.sync(self.api.pr), "merged_policy_refreshed")
        self.assertTrue(self.worker.policy_merged)
        self.assertEqual(self.worker.recovery_needed, set())
        self.assertIn(("dispatch_ci", "POST", {"ref": "main"}), self.api.calls)
        self.assertIn(("dispatch", "POST", {"ref": "main"}), self.api.calls)

    def test_incomplete_confirmation_schedules_bounded_durable_recovery(self):
        for attempt in [0, 3]:
            with self.subTest(attempt=attempt):
                api = FakeGitHub();api.files = [{"filename": "AGENTS.md"}]
                original = api.call
                merged = False
                def incomplete(path, method="GET", payload=None):
                    nonlocal merged
                    if path.endswith("/merge"):
                        merged = True
                        api.pr.update(merged=True, state="closed")
                        return self.incomplete_http_response(path, method, payload)
                    if merged and path == "repos/owner/repo/pulls/1":
                        return self.incomplete_http_response(path, method, payload)
                    return original(path, method, payload)
                with tempfile.NamedTemporaryFile(mode="w+") as f:
                    json.dump({"repository": {"default_branch": "main"},
                               "inputs": {"auto_merge_recovery_retry": str(attempt)}}, f);f.flush()
                    with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name,
                                                 "NIMO_GATE_BOOTSTRAP_NUMBERS": "[1]"}), \
                         patch("auto_merge.MergeGitHub", return_value=api), \
                         patch.object(api, "call", side_effect=incomplete), patch("auto_merge.time.sleep"):
                        with self.assertRaises(RuntimeError):
                            run()
                retries = [c for c in api.calls if c[0] == "retry"]
                self.assertEqual(len(retries), int(attempt == 0))
                if retries:
                    self.assertEqual(retries[0][2], {"attempt": 0, "numbers": [1], "recovery_attempt": 1})
                self.assertFalse(any(c[0] in {"dispatch", "dispatch_ci"} for c in api.calls))

    def test_mixed_mergeability_and_recovery_keep_every_permitted_retry(self):
        for policy_changed in [False, True]:
            with self.subTest(policy_changed=policy_changed):
                api = FakeGitHub();api.ci = True
                original_call, original_pages = api.call, api.pages
                merged = False
                def mixed(path, method="GET", payload=None):
                    nonlocal merged
                    if path == "graphql" and method == "POST" and payload["query"].startswith("query"):
                        info = copy.deepcopy(api.info)
                        if payload["variables"]["number"] == 1:
                            info["mergeStateStatus"] = "UNKNOWN"
                        return {"data": {"repository": {"pullRequest": info}}}
                    if path.endswith("/pulls/2/merge"):
                        merged = True
                        return self.incomplete_http_response(path, method, payload)
                    if path == "repos/owner/repo/pulls/2":
                        if merged:
                            return self.incomplete_http_response(path, method, payload)
                        return {**copy.deepcopy(api.pr), "number": 2}
                    return original_call(path, method, payload)
                def files(path, **kwargs):
                    if path.endswith("/pulls/2/files"):
                        return [{"filename": "AGENTS.md" if policy_changed else "README.md"}]
                    return original_pages(path, **kwargs)
                with tempfile.NamedTemporaryFile(mode="w+") as f:
                    json.dump({"repository": {"default_branch": "main"}}, f);f.flush()
                    with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name,
                                                 "NIMO_GATE_BOOTSTRAP_NUMBERS": "[]"}), \
                         patch("auto_merge.MergeGitHub", return_value=api), \
                         patch.object(api, "call", side_effect=mixed), \
                         patch.object(api, "pages", side_effect=files), patch("auto_merge.time.sleep"):
                        with self.assertRaises(RuntimeError):
                            run()
                retries = [c for c in api.calls if c[0] == "retry"]
                self.assertEqual(len(retries), 1)
                self.assertEqual(retries[0][2], {"attempt": int(not policy_changed), "recovery_attempt": 1,
                    "numbers": [2] if policy_changed else [1, 2]})
                expected_state = {"2": {"mergeability": 0, "recovery": 1}}
                if not policy_changed:
                    expected_state["1"] = {"mergeability": 1, "recovery": 0}
                self.assertEqual(api.retry_states[-1], expected_state)


    def test_first_unknown_merge_response_has_budget_after_final_mergeability_poll(self):
        api = FakeGitHub();api.files = [{"filename": "AGENTS.md"}]
        original = api.call
        def uncertain(path, method="GET", payload=None):
            if path.endswith("/merge"):
                return self.incomplete_http_response(path, method, payload)
            return original(path, method, payload)
        with tempfile.NamedTemporaryFile(mode="w+") as f:
            json.dump({"repository": {"default_branch": "main"},
                       "inputs": {"auto_merge_retry": "3"}}, f);f.flush()
            with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name,
                                         "NIMO_GATE_BOOTSTRAP_NUMBERS": "[1]"}), \
                 patch("auto_merge.MergeGitHub", return_value=api), \
                 patch.object(api, "call", side_effect=uncertain), patch("auto_merge.time.sleep"):
                with self.assertRaises(RuntimeError):
                    run()
        retries = [c for c in api.calls if c[0] == "retry"]
        self.assertEqual(len(retries), 1)
        self.assertEqual(retries[0][2], {"attempt": 3, "recovery_attempt": 1, "numbers": [1]})

    def test_merge_response_recovery_budget_is_carried_and_stops_after_three_dispatches(self):
        api = FakeGitHub();api.files = [{"filename": "AGENTS.md"}]
        original = api.call
        def uncertain(path, method="GET", payload=None):
            if path.endswith("/merge"):
                return self.incomplete_http_response(path, method, payload)
            return original(path, method, payload)
        recovery = 0
        for _ in range(4):
            api.calls.clear()
            with tempfile.NamedTemporaryFile(mode="w+") as f:
                json.dump({"repository": {"default_branch": "main"}, "inputs": {
                    "auto_merge_retry": "3", "auto_merge_recovery_retry": str(recovery)}}, f);f.flush()
                with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name,
                                             "NIMO_GATE_BOOTSTRAP_NUMBERS": "[1]"}), \
                     patch("auto_merge.MergeGitHub", return_value=api), \
                     patch.object(api, "call", side_effect=uncertain), patch("auto_merge.time.sleep"):
                    with self.assertRaises(RuntimeError):
                        run()
            retries = [c for c in api.calls if c[0] == "retry"]
            if recovery == 3:
                self.assertEqual(retries, [])
            else:
                self.assertEqual(len(retries), 1)
                self.assertEqual(retries[0][2], {"attempt": 3, "recovery_attempt": recovery + 1, "numbers": [1]})
                recovery = retries[0][2]["recovery_attempt"]

    def test_mixed_retry_budgets_dispatch_only_categories_with_remaining_attempts(self):
        for retry, recovery in [(0, 0), (3, 0), (0, 3), (3, 3)]:
            with self.subTest(retry=retry, recovery=recovery):
                api = FakeGitHub();api.ci = True
                original_call, original_pages = api.call, api.pages
                def mixed(path, method="GET", payload=None):
                    if path == "graphql" and method == "POST" and payload["query"].startswith("query"):
                        info = copy.deepcopy(api.info)
                        if payload["variables"]["number"] == 1:
                            info["mergeStateStatus"] = "UNKNOWN"
                        return {"data": {"repository": {"pullRequest": info}}}
                    if path.endswith("/pulls/2/merge"):
                        return self.incomplete_http_response(path, method, payload)
                    if path == "repos/owner/repo/pulls/2":
                        return {**copy.deepcopy(api.pr), "number": 2}
                    return original_call(path, method, payload)
                def files(path, **kwargs):
                    if path.endswith("/pulls/2/files"):
                        return [{"filename": "README.md"}]
                    return original_pages(path, **kwargs)
                with tempfile.NamedTemporaryFile(mode="w+") as f:
                    json.dump({"repository": {"default_branch": "main"}, "inputs": {
                        "auto_merge_retry": str(retry), "auto_merge_recovery_retry": str(recovery)}}, f);f.flush()
                    with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name,
                                                 "NIMO_GATE_BOOTSTRAP_NUMBERS": "[]"}), \
                         patch("auto_merge.MergeGitHub", return_value=api), \
                         patch.object(api, "call", side_effect=mixed), \
                         patch.object(api, "pages", side_effect=files), patch("auto_merge.time.sleep"):
                        with self.assertRaises(RuntimeError):
                            run()
                retries = [c for c in api.calls if c[0] == "retry"]
                numbers = ([1] if retry < 3 else []) + ([2] if recovery < 3 else [])
                if not numbers:
                    self.assertEqual(retries, [])
                else:
                    self.assertEqual(len(retries), 1)
                    expected = {"attempt": retry + int(retry < 3), "numbers": numbers,
                                "recovery_attempt": recovery + int(recovery < 3)}
                    self.assertEqual(retries[0][2], expected)

    def test_retry_dispatch_contract_carries_two_independent_counters(self):
        api = MergeGitHub.__new__(MergeGitHub)
        with patch.object(api, "dispatch_workflow") as dispatch:
            api.dispatch_retry("main", 3, {2, 1}, recovery_attempt=1)
            dispatch.assert_called_once_with("review-gate.yml", "main", {
                "auto_merge_retry": "3", "auto_merge_recovery_retry": "1", "auto_merge_numbers": "[1, 2]"})
        with patch.object(api, "dispatch_workflow") as dispatch:
            state = {"1": {"mergeability": 3, "recovery": 0},
                     "2": {"mergeability": 0, "recovery": 1}}
            api.dispatch_retry("main", 3, {2, 1}, recovery_attempt=1, retry_state=state)
            sent = dispatch.call_args.args[2]
            self.assertEqual(json.loads(sent["auto_merge_retry_state"]), state)
        import base64
        from pathlib import Path
        config = MergeGitHub.__new__(MergeGitHub);config.repo = "owner/repo"
        path = Path(__file__).resolve().with_name("review-gate.yml")
        if not path.exists():
            path = path.parent.parent / "workflows" / path.name
        with patch.object(config, "call", return_value={"content": base64.b64encode(path.read_bytes()).decode()}):
            inputs = config.workflow("review-gate.yml", "main")["on"]["workflow_dispatch"]["inputs"]
        self.assertEqual(inputs["auto_merge_recovery_retry"]["default"], "0")
        self.assertEqual(inputs["auto_merge_recovery_retry"]["required"], "false")
        self.assertEqual(inputs["auto_merge_retry_state"]["default"], "{}")
        self.assertEqual(inputs["auto_merge_retry_state"]["required"], "false")


    def test_another_pr_exhausting_recovery_does_not_consume_first_lost_put_budget(self):
        api = FakeGitHub();api.ci = True
        original_call, original_pages = api.call, api.pages
        turn = 0
        def mixed(path, method="GET", payload=None):
            if path == "graphql" and method == "POST" and payload["query"].startswith("query"):
                info = copy.deepcopy(api.info)
                if payload["variables"]["number"] == 2 and turn < 3:
                    info["mergeStateStatus"] = "UNKNOWN"
                return {"data": {"repository": {"pullRequest": info}}}
            if path.endswith("/merge"):
                return self.incomplete_http_response(path, method, payload)
            if path == "repos/owner/repo/pulls/2":
                return {**copy.deepcopy(api.pr), "number": 2}
            return original_call(path, method, payload)
        def files(path, **kwargs):
            if path.endswith("/pulls/2/files"):
                return [{"filename": "README.md"}]
            return original_pages(path, **kwargs)
        state, scope, legacy_retry, legacy_recovery = {}, [], "0", "0"
        observed = []
        for turn in range(7):
            api.calls.clear();api.retry_states.clear()
            with tempfile.NamedTemporaryFile(mode="w+") as f:
                json.dump({"repository": {"default_branch": "main"}, "inputs": {
                    "auto_merge_retry_state": json.dumps(state), "auto_merge_retry": legacy_retry,
                    "auto_merge_recovery_retry": legacy_recovery}}, f);f.flush()
                with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name,
                    "NIMO_GATE_BOOTSTRAP_NUMBERS": json.dumps(scope), "NIMO_GATE_BOOTSTRAP_AFFECTED_HEADS": "[]"}), \
                     patch("auto_merge.MergeGitHub", return_value=api), \
                     patch.object(api, "call", side_effect=mixed), \
                     patch.object(api, "pages", side_effect=files), patch("auto_merge.time.sleep"):
                    with self.assertRaises(RuntimeError):
                        run()
            dispatches = [c for c in api.calls if c[0] == "retry"]
            if turn == 6:
                self.assertEqual(dispatches, [])
                continue
            self.assertEqual(len(dispatches), 1)
            state = api.retry_states[-1]
            payload = dispatches[0][2]
            scope = payload["numbers"]
            legacy_retry, legacy_recovery = str(payload["attempt"]), str(payload.get("recovery_attempt", 0))
            observed.append(copy.deepcopy(state))
            self.assertEqual(state["1"], {"mergeability": 0, "recovery": min(turn + 1, 3)})
            self.assertEqual(state["2"], {"mergeability": min(turn + 1, 3), "recovery": max(turn - 2, 0)})
            self.assertEqual(scope, [1, 2] if turn < 3 else [2])
        self.assertEqual(observed[3]["2"], {"mergeability": 3, "recovery": 1})
        self.assertEqual(observed[-1]["2"]["recovery"], 3)

    def test_distinct_per_pr_budgets_preserve_exhausted_entries_outside_next_scope(self):
        api = FakeGitHub();api.ci = True
        original_call, original_pages = api.call, api.pages
        def mixed(path, method="GET", payload=None):
            if path == "graphql" and method == "POST" and payload["query"].startswith("query"):
                info = copy.deepcopy(api.info)
                if payload["variables"]["number"] == 1:
                    info["mergeStateStatus"] = "UNKNOWN"
                return {"data": {"repository": {"pullRequest": info}}}
            if path.endswith("/pulls/2/merge"):
                return self.incomplete_http_response(path, method, payload)
            if path == "repos/owner/repo/pulls/2":
                return {**copy.deepcopy(api.pr), "number": 2}
            return original_call(path, method, payload)
        def files(path, **kwargs):
            if path.endswith("/pulls/2/files"):
                return [{"filename": "README.md"}]
            return original_pages(path, **kwargs)
        state = {"1": {"mergeability": 2, "recovery": 3},
                 "2": {"mergeability": 3, "recovery": 1}}
        for turn, wanted_scope in [(0, [1, 2]), (1, [2])]:
            api.calls.clear();api.retry_states.clear()
            with tempfile.NamedTemporaryFile(mode="w+") as f:
                json.dump({"repository": {"default_branch": "main"}, "inputs": {
                    "auto_merge_retry_state": json.dumps(state), "auto_merge_retry": "3",
                    "auto_merge_recovery_retry": "3"}}, f);f.flush()
                with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name,
                                             "NIMO_GATE_BOOTSTRAP_NUMBERS": "[]"}), \
                     patch("auto_merge.MergeGitHub", return_value=api), \
                     patch.object(api, "call", side_effect=mixed), \
                     patch.object(api, "pages", side_effect=files), patch("auto_merge.time.sleep"):
                    with self.assertRaises(RuntimeError):
                        run()
            dispatches = [c for c in api.calls if c[0] == "retry"]
            self.assertEqual(len(dispatches), 1)
            self.assertEqual(dispatches[0][2]["numbers"], wanted_scope)
            state = api.retry_states[-1]
            self.assertEqual(state["1"], {"mergeability": 3, "recovery": 3})
            self.assertEqual(state["2"], {"mergeability": 3, "recovery": turn + 2})

    def test_malformed_retry_state_is_rejected_before_any_github_operation(self):
        malformed = ["[]", "null", "{", json.dumps({"0": {"mergeability": 0, "recovery": 0}}),
            json.dumps({"01": {"mergeability": 0, "recovery": 0}}),
            json.dumps({"1": {"mergeability": True, "recovery": 0}}),
            json.dumps({"1": {"mergeability": -1, "recovery": 0}}),
            json.dumps({"1": {"mergeability": 0, "recovery": 4}}),
            json.dumps({"1": {"mergeability": 0, "recovery": "1"}}),
            json.dumps({"1": {"mergeability": 0}}),
            json.dumps({"1": {"mergeability": 0, "recovery": 0, "extra": 0}}),
            '{"1":{"mergeability":0,"recovery":0},"1":{"mergeability":0,"recovery":0}}',
            '{"1":{"mergeability":0,"mergeability":1,"recovery":0}}']
        for raw in malformed:
            with self.subTest(raw=raw), tempfile.NamedTemporaryFile(mode="w+") as f:
                json.dump({"repository": {"default_branch": "main"}, "inputs": {
                    "auto_merge_retry_state": raw}}, f);f.flush()
                with patch.dict(os.environ, {"GITHUB_EVENT_PATH": f.name}), \
                     patch("auto_merge.MergeGitHub") as api:
                    with self.assertRaises(RuntimeError):
                        run()
                    api.assert_not_called()

if __name__ == "__main__":
    unittest.main()
