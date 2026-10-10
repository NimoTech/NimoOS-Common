#!/usr/bin/env python3
"""Squash reviewed PRs using the Actions token and the existing branch rules."""
import json
import datetime
import os
import time
import base64
import re
import yaml
from contextlib import contextmanager
import urllib.error
import urllib.request
import urllib.parse

from review_gate import BRIDGE, BRIDGE_ID, GitHub, SupersededEvaluation, checked_decision, run as refresh_review_gate


def changes_policy(paths):
    return any(path in {"AGENTS.md", ".github/CODEOWNERS"}
               or path.startswith(".github/scripts/")
               or path.startswith(".github/workflows/")
               for path in paths)


class MergeGitHub(GitHub):
    def workflow(self, filename, ref, required=False):
        try:
            entry = self.call(f"repos/{self.repo}/contents/.github/workflows/{filename}?ref=" + urllib.parse.quote(ref, safe=""))
        except urllib.error.HTTPError as error:
            if error.code == 404 and not required:
                return None
            raise
        # YAML 1.1 calls 'on' a boolean. Disable only that resolver so GitHub's
        # event key is interpreted as a string; safe loading never executes tags.
        class Loader(yaml.SafeLoader):
            yaml_implicit_resolvers = {k: [(tag, regex) for tag, regex in values
                if tag != 'tag:yaml.org,2002:bool']
                for k, values in yaml.SafeLoader.yaml_implicit_resolvers.items()}
        config = yaml.load(base64.b64decode(entry["content"]).decode(), Loader=Loader)
        if not isinstance(config, dict):
            raise RuntimeError("Candidate workflow is not a YAML mapping")
        return config

    @staticmethod
    def events(config):
        value = config.get("on", {})
        if isinstance(value, str):
            return {value}
        if isinstance(value, dict):
            return set(value)
        if isinstance(value, list) and all(isinstance(x, str) for x in value):
            return set(value)
        raise RuntimeError("Invalid workflow event declaration")

    def has_ci(self, ref):
        config = self.workflow("ci.yml", ref)
        if config is None:
            return False
        if config.get("name") != "CI" or not {"workflow_dispatch", "pull_request"} <= self.events(config):
            raise RuntimeError("Candidate CI must retain name CI, pull_request and workflow_dispatch")
        return True

    def verify_gate(self, ref):
        config = self.workflow("review-gate.yml", ref, required=True)
        if config.get("name") != "Review Merge Gate" or "workflow_dispatch" not in self.events(config):
            raise RuntimeError("Candidate gate must retain its name and dispatch event")

    def verify_bridge(self, ref):
        config = self.workflow("review-gate-events.yml", ref, required=True)
        if (config.get("name") != "Review Gate Events" or
                not {"pull_request_review", "pull_request_review_comment"} <= self.events(config)):
            raise RuntimeError("Candidate review event bridge must retain its name and review events")

    def verify_candidate_workflow(self, ref):
        config = self.workflow("review-policy-tests.yml", ref, required=True)
        expected = 'Candidate review policy for PR #${{ github.event.pull_request.number }} at ${{ github.event.pull_request.head.sha }}'
        if (config.get("name") != "Review Policy Tests" or config.get("run-name") != expected
                or "pull_request" not in self.events(config)
                or config.get("permissions") != {"contents": "read"}
                or not isinstance(config.get("jobs"), dict) or not config["jobs"]):
            raise RuntimeError("Candidate policy tests must retain their trusted identity and read-only permissions")

    def candidate_verified(self, pr):
        # Metadata comes from the attested read-only pull_request workflow, never a
        # check name supplied by candidate code. Candidate tests run on a separate
        # read-only runner, with no write token, secrets or persisted credentials.
        # Policy tests cover the head snapshot. It must include the current main
        # so two separately tested policy branches cannot combine untested code.
        base = self.call(f"repos/{self.repo}/git/ref/heads/" + urllib.parse.quote(pr["base"]["ref"], safe=""))["object"]["sha"]
        comparison = self.call(f"repos/{self.repo}/compare/{base}...{pr['head']['sha']}")
        if comparison.get("status") not in {"ahead", "identical"}:
            return False
        path = ".github/workflows/review-policy-tests.yml"
        main_source = self.call(f"repos/{self.repo}/contents/{path}?ref=" + urllib.parse.quote(pr["base"]["ref"], safe=""))
        candidate_source = self.call(f"repos/{self.repo}/contents/{path}?ref=" + pr["head"]["sha"])
        if main_source["sha"] != candidate_source["sha"]:
            # A modified test runner cannot attest itself. Its own maintenance
            # requires an ordinary maintainer merge under the existing rules.
            return False
        workflow = self.call(f"repos/{self.repo}/actions/workflows/review-policy-tests.yml")
        title = f"Candidate review policy for PR #{pr['number']} at {pr['head']['sha']}"
        runs = self.pages(f"repos/{self.repo}/actions/workflows/{workflow['id']}/runs?event=pull_request&head_sha=" + pr["head"]["sha"], key="workflow_runs")
        matching = [run for run in runs if run.get("workflow_id") == workflow['id']
                    and run.get("event") == "pull_request" and run.get("display_title") == title
                    and run.get("path", "").split('@')[0] == ".github/workflows/review-policy-tests.yml"]
        if not matching:
            return False
        latest = max(matching, key=lambda x: (x['id'], x.get('run_attempt', 1)))
        if latest.get('status') != 'completed' or latest.get('conclusion') != 'success':
            return False
        jobs = self.pages(f"repos/{self.repo}/actions/runs/{latest['id']}/attempts/{latest.get('run_attempt', 1)}/jobs", key="jobs")
        candidates = [job for job in jobs if job.get("name") == "candidate"]
        return (len(candidates) == 1 and candidates[0].get("status") == "completed"
                and candidates[0].get("conclusion") == "success"
                and any(step.get("name") == "Test candidate policy without privileged credentials"
                        and step.get("status") == "completed" and step.get("conclusion") == "success"
                        for step in candidates[0].get("steps", [])))

    def followup(self, pr, state, description):
        return self.call(f"repos/{self.repo}/statuses/{pr['head']['sha']}", "POST", {
            "context": "Nimo Merge Followup", "state": state, "description": description})

    def recover_followups(self, default):
        # The intent is committed before PUT /merge; a failed runner or rerun
        # can find it even though the PR is now closed. Historical owner/Claude
        # merges are ignored without making a request for their commit statuses.
        failures = []
        owner, name = self.repo.split("/")
        query = """query($owner:String!,$name:String!,$cursor:String) {
          repository(owner:$owner,name:$name) {
            pullRequests(first:100,after:$cursor,states:MERGED,orderBy:{field:UPDATED_AT,direction:DESC}) {
              pageInfo {hasNextPage endCursor}
              nodes { number baseRefName headRefOid mergedBy {login ... on Bot {databaseId}}
                commits(last:1) {nodes {commit {status {contexts {
                  context state description creator {login ... on Bot {databaseId}}
                }}}}}
              }
            }
          }
        }"""
        cursor = None
        for _ in range(100):
            result = self.call("graphql", "POST", {"query": query,
                "variables": {"owner": owner, "name": name, "cursor": cursor}})
            if result.get("errors"):
                raise RuntimeError("Cannot inspect durable merge followups: " + str(result["errors"]))
            page = result["data"]["repository"]["pullRequests"]
            for row in page["nodes"]:
                actor = row.get("mergedBy") or {}
                if (row["baseRefName"] != default or actor.get("databaseId") != BRIDGE_ID
                        or actor.get("login", "").removesuffix("[bot]") != BRIDGE.removesuffix("[bot]")):
                    continue
                commits = row["commits"]["nodes"]
                status = commits[-1]["commit"].get("status") if commits else None
                intent = next((x for x in (status or {}).get("contexts", [])
                               if x["context"] == "Nimo Merge Followup"), None)
                actor = (intent or {}).get("creator") or {}
                if (not intent or intent["state"] != "PENDING"
                        or actor.get("databaseId") != BRIDGE_ID or actor.get("login", "").removesuffix("[bot]") != BRIDGE.removesuffix("[bot]")):
                    continue
                wanted = intent.get("description", "")
                if wanted not in {"v1 ci", "v1 gate", "v1 ci gate"}:
                    raise RuntimeError("Unknown durable merge followup intent")
                pr = {"number": row["number"], "head": {"sha": row["headRefOid"]}}
                try:
                    self.finish_followup(pr, default, "ci" in wanted.split(), "gate" in wanted.split(), recovering=True)
                except Exception as error:
                    failures.append(pr["number"])
                    print("PR #" + str(pr["number"]), "followup retry failed:", str(error))
            if not page["pageInfo"]["hasNextPage"]:
                break
            cursor = page["pageInfo"]["endCursor"]
        else:
            raise RuntimeError("Merged PR pagination exceeded the safe bound")
        if failures:
            raise RuntimeError("Merged PR followups still pending: " + str(failures))

    def refresh_gate(self):
        # This process was checked out from the current trusted default branch.
        # Recover policy refresh in-place rather than dispatching another gate:
        # a persistent CI/HTTP failure must never create a self-sustaining run chain.
        names = [k for k in os.environ if k.startswith("NIMO_GATE_BOOTSTRAP_")]
        previous = {k: os.environ.pop(k) for k in names}
        # An empty known-head snapshot makes GitHub.check initialize every
        # open head before evaluation, using its existing race-safe status API.
        # Clearing the PR filter alone would skip heads owned by older epochs.
        os.environ["NIMO_GATE_BOOTSTRAP_HEADS"] = "[]"
        os.environ["NIMO_GATE_BOOTSTRAP_NUMBERS"] = "[]"
        os.environ["NIMO_GATE_BOOTSTRAP_STARTED_AT"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        try:
            refresh_review_gate()
        finally:
            for key in list(os.environ):
                if key.startswith("NIMO_GATE_BOOTSTRAP_"):
                    del os.environ[key]
            os.environ.update(previous)

    def finish_followup(self, pr, default, ci, gate, recovering=False):
        errors = []
        remaining = {"ci": ci, "gate": gate}
        for task in ["ci", "gate"]:
            if not remaining[task]:
                continue
            try:
                if task == "gate" and recovering:
                    self.refresh_gate()
                else:
                    self.dispatch_workflow("ci.yml" if task == "ci" else "review-gate.yml", default)
                remaining[task] = False
                wanted = [name for name, pending in remaining.items() if pending]
                self.followup(pr, "pending" if wanted else "success",
                    "v1 " + " ".join(wanted) if wanted else "Requested main CI and policy refresh completed.")
            except Exception as error:
                errors.append(str(error))
        if errors:
            raise RuntimeError("Merge completed; durable followup awaits retry: " + "; ".join(errors))

    def dispatch_retry(self, default, attempt, numbers):
        self.dispatch_workflow("review-gate.yml", default, {"auto_merge_retry": str(attempt), "auto_merge_numbers": json.dumps(sorted(numbers))})

    def dispatch_workflow(self, workflow, default, inputs=None):
        # Dispatch is one of the events GitHub permits GITHUB_TOKEN to trigger.
        # Its successful REST response has no JSON body.
        assert workflow in {"review-gate.yml", "ci.yml"}
        req = urllib.request.Request(self.api + f"/repos/{self.repo}/actions/workflows/{workflow}/dispatches",
            method="POST", data=json.dumps({"ref": default, **({"inputs": inputs} if inputs else {})}).encode(),
            headers={"Authorization": "Bearer " + self.token,
                     "Accept": "application/vnd.github+json", "Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=25) as response:
            if response.status != 204:
                raise RuntimeError("GitHub did not confirm the default-branch gate refresh.")

    def dispatch_gate(self, default):
        self.dispatch_workflow("review-gate.yml", default)

    def dispatch_ci(self, default):
        self.dispatch_workflow("ci.yml", default)


def eligible(pr, default):
    return (pr.get("state") == "open" and not pr.get("draft")
            and pr.get("base", {}).get("ref") == default
            and not any(label["name"].lower() == "hold" for label in pr.get("labels", [])))


def unchanged(before, after):
    return (after.get("state") == "open"
            and before["head"]["sha"] == after["head"]["sha"]
            and before["base"]["ref"] == after["base"]["ref"]
            and before.get("draft") == after.get("draft")
            and before.get("updated_at") == after.get("updated_at")
            and sorted(x["name"] for x in before.get("labels", []))
                == sorted(x["name"] for x in after.get("labels", [])))


class AutoMerge:
    def __init__(self, api, default):
        self.api, self.default = api, default
        self.policy_merged = False
        self.retry_needed = set()

    def graphql(self, query, variables):
        response = self.api.call("graphql", "POST", {"query": query, "variables": variables})
        if response.get("errors"):
            raise RuntimeError("GitHub rejected the auto-merge operation: "
                               + "; ".join(e.get("message", "GraphQL error") for e in response["errors"]))
        return response["data"]

    def inspect(self, number):
        owner, name = self.api.repo.split("/")
        query = """query($owner:String!,$name:String!,$number:Int!) {
          repository(owner:$owner,name:$name) {
            pullRequest(number:$number) {
              id state isDraft headRefOid baseRefName mergeStateStatus reviewDecision
              autoMergeRequest { mergeMethod }
            }
          }
        }"""
        return self.graphql(query, {"owner": owner, "name": name, "number": number})["repository"]["pullRequest"]

    def disable(self, pr):
        if not pr.get("auto_merge"):
            return "not_registered"
        query = """mutation($id:ID!) {
          disablePullRequestAutoMerge(input:{pullRequestId:$id}) { pullRequest {id} }
        }"""
        self.graphql(query, {"id": pr["node_id"]})
        return "disabled"

    def sync(self, snapshot, cancelled=False):
        n = snapshot["number"]
        pr = self.api.call(f"repos/{self.api.repo}/pulls/{n}")
        if not eligible(pr, self.default):
            return self.disable(pr)
        if pr.get("auto_merge"):
            if cancelled:
                self.disable(pr)
                raise RuntimeError("Auto-merge was registered again during validation; retry required.")
            self.disable(pr)
            return self.sync(pr, cancelled=True)
        sha = pr["head"]["sha"]
        # A passing status must be ours, must refer to this head, and must still
        # belong to the event that just validated it. No native summary shortcut.
        history = self.api.status_history(sha)
        if (not history or history[0].get("state") != "success"
                or history[0].get("creator", {}).get("id") != BRIDGE_ID
                or history[0].get("creator", {}).get("login") != BRIDGE):
            return "waiting_for_review_gate"
        self.api.assert_current_epoch(sha, history=history)
        state, _ = checked_decision(self.api, pr)
        if state != "success":
            return "waiting_for_valid_review"
        info = self.inspect(n)
        for _ in range(3):
            if not info or info.get("mergeStateStatus") != "UNKNOWN":
                break
            time.sleep(2)
            info = self.inspect(n)
        if (not info or info["state"] != "OPEN" or info["isDraft"]
                or info["headRefOid"] != sha or info["baseRefName"] != self.default):
            return "changed"
        current = self.api.call(f"repos/{self.api.repo}/pulls/{n}")
        if not unchanged(pr, current) or not eligible(current, self.default):
            return "changed"
        self.api.assert_current_epoch(sha)
        if info.get("autoMergeRequest"):
            if cancelled:
                self.disable({**current, "node_id": info["id"], "auto_merge": info["autoMergeRequest"]})
                raise RuntimeError("Auto-merge was registered again during validation; retry required.")
            self.disable({**current, "node_id": info["id"], "auto_merge": info["autoMergeRequest"]})
            return self.sync(current, cancelled=True)
        if info.get("mergeStateStatus") in {"CLEAN", "UNSTABLE", "BEHIND"}:
            # GitHub does not offer auto-merge registration for an immediately
            # mergeable PR. Use the ordinary, SHA-bound merge endpoint instead.
            # The Actions App has no ruleset bypass and GitHub enforces all rules.
            if info.get("reviewDecision") != "APPROVED":
                return "waiting_for_formal_approval"
            files = self.api.pages(f"repos/{self.api.repo}/pulls/{n}/files")
            paths = {f[key] for f in files for key in ("filename", "previous_filename") if f.get(key)}
            if pr.get("changed_files") is not None and pr["changed_files"] != len(files):
                raise RuntimeError("PR file list is incomplete; automatic merge refused")
            policy_changed = changes_policy(paths)
            ci_changed = ".github/workflows/ci.yml" in paths
            # CI edits come from the candidate head. Otherwise a branch made
            # before this rollout inherits CI from the current default branch.
            has_ci = self.api.has_ci(sha if ci_changed else self.default)
            if policy_changed and not self.api.candidate_verified(pr):
                return "waiting_for_candidate_policy_tests"
            if ".github/workflows/review-policy-tests.yml" in paths:
                self.api.verify_candidate_workflow(sha)
            if ".github/workflows/review-gate-events.yml" in paths:
                self.api.verify_bridge(sha)
            if policy_changed:
                self.api.verify_gate(sha if ".github/workflows/review-gate.yml" in paths else self.default)
            if has_ci or policy_changed:
                self.api.followup(pr, "pending", "v1" + (" ci" if has_ci else "") + (" gate" if policy_changed else ""))
            # File pagination is another API boundary. Verify again before merge.
            current = self.api.call(f"repos/{self.api.repo}/pulls/{n}")
            if current.get("auto_merge"):
                self.disable(current)
                return "changed"
            if not unchanged(pr, current) or not eligible(current, self.default):
                return "changed"
            self.api.assert_current_epoch(sha)
            state, _ = checked_decision(self.api, current)
            if state != "success":
                return "waiting_for_valid_review"
            if policy_changed and not self.api.candidate_verified(current):
                return "waiting_for_candidate_policy_tests"
            self.api.assert_current_epoch(sha)
            final = self.api.call(f"repos/{self.api.repo}/pulls/{n}")
            if final.get("auto_merge"):
                self.disable(final)
                return "changed"
            if not unchanged(current, final) or not eligible(final, self.default):
                return "changed"
            # The GitHub API binds this PUT to the head SHA and native branch
            # rules. Labels/custom evidence have no atomic compare-and-merge
            # API: this is the final observed snapshot, not a transactional lock
            # against edits arriving after this read. Never grant this bot bypass.
            if policy_changed:
                self.policy_merged = True  # Also stop on a lost/ambiguous merge response.
            result = self.api.call(f"repos/{self.api.repo}/pulls/{n}/merge", "PUT", {
                "sha": sha, "merge_method": "squash"})
            if not result.get("merged"):
                raise RuntimeError("GitHub did not complete the protected squash merge.")
            if policy_changed:
                self.policy_merged = True
            if has_ci or policy_changed:
                self.api.finish_followup(pr, self.default, has_ci, policy_changed)
            return "merged_policy_refreshed" if policy_changed else "merged"
        # A persistent native request could act on an old green verdict before
        # a hold/finding event invalidates it. Wait for the next trusted event,
        # then validate the current state and merge directly instead.
        if info.get("mergeStateStatus") == "UNKNOWN":
            self.retry_needed.add(n)
        return "waiting_for_github_requirements"


def run():
    with open(os.environ["GITHUB_EVENT_PATH"], encoding="utf-8") as f:
        event = json.load(f)
    api = MergeGitHub()
    default = event["repository"]["default_branch"]
    meta = api.call(f"repos/{api.repo}")
    if meta["default_branch"] != default or not meta.get("allow_auto_merge"):
        raise RuntimeError("Repository auto-merge is disabled or the default branch changed.")
    api.recover_followups(default)
    pulls = api.pages(f"repos/{api.repo}/pulls?state=open")
    numbers = set(json.loads(os.environ.get("NIMO_GATE_BOOTSTRAP_NUMBERS", "[]")))
    heads = set(json.loads(os.environ.get("NIMO_GATE_BOOTSTRAP_AFFECTED_HEADS", "[]")))
    if numbers:
        pulls = [p for p in pulls if p["number"] in numbers or p["head"]["sha"] in heads]
    worker, failures = AutoMerge(api, default), []
    for pr in pulls:
        if pr["base"]["ref"] != default:
            continue
        try:
            print("PR #" + str(pr["number"]), worker.sync(pr))
        except SupersededEvaluation:
            print("PR #" + str(pr["number"]), "newer review event owns this head; skipping")
        except Exception as error:
            failures.append(pr["number"])
            print("PR #" + str(pr["number"]), "auto-merge failed:", str(error))
        if worker.policy_merged:
            print("Review policy changed; remaining PRs await a new default-branch evaluation.")
            break
    if worker.retry_needed and not worker.policy_merged:
        retry = int(event.get("inputs", {}).get("auto_merge_retry", "0"))
        if retry not in range(4):
            raise RuntimeError("Invalid mergeability retry count")
        if retry < 3:
            time.sleep(10)
            api.dispatch_retry(default, retry + 1, worker.retry_needed)
        else:
            raise RuntimeError("GitHub mergeability remains unknown after bounded retries; rerun this workflow.")
    if failures:
        raise RuntimeError("Auto-merge needs retry for PRs: " + ", ".join(map(str, failures)))


if __name__ == "__main__":
    run()
