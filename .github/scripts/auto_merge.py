#!/usr/bin/env python3
"""Squash reviewed PRs using the Actions token and the existing branch rules."""
import json
import os
import time
import base64
import re
import urllib.error
import urllib.request
import urllib.parse

from review_gate import BRIDGE, BRIDGE_ID, GitHub, SupersededEvaluation, checked_decision


def changes_policy(paths):
    return any(path in {"AGENTS.md", ".github/CODEOWNERS"}
               or path.startswith(".github/scripts/")
               or (path.startswith(".github/workflows/review-gate") and path.endswith(".yml"))
               for path in paths)


class MergeGitHub(GitHub):
    def dispatchable(self, workflow, ref, required=False):
        try:
            entry = self.call(f"repos/{self.repo}/contents/.github/workflows/{workflow}?ref=" + urllib.parse.quote(ref, safe=""))
        except urllib.error.HTTPError as error:
            if error.code == 404 and not required:
                return False
            raise
        body = base64.b64decode(entry["content"]).decode()
        if not re.search(r"^  workflow_dispatch:", body, re.M):
            raise RuntimeError("Candidate workflow must support workflow_dispatch before automatic merge.")
        return True

    def has_ci(self, ref):
        return self.dispatchable("ci.yml", ref)

    def verify_gate(self, ref):
        self.dispatchable("review-gate.yml", ref, required=True)

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
                    self.finish_followup(pr, default, "ci" in wanted.split(), "gate" in wanted.split())
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

    def finish_followup(self, pr, default, ci, gate):
        errors = []
        for workflow, wanted in [("ci.yml", ci), ("review-gate.yml", gate)]:
            if wanted:
                try:
                    self.dispatch_workflow(workflow, default)
                except Exception as error:
                    errors.append(str(error))
        if errors:
            raise RuntimeError("Merge completed; durable followup awaits retry: " + "; ".join(errors))
        self.followup(pr, "success", "Requested main CI and review-policy refresh completed.")

    def dispatch_retry(self, default, attempt):
        self.dispatch_workflow("review-gate.yml", default, {"auto_merge_retry": str(attempt)})

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
        self.retry_needed = False

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
            policy_changed = changes_policy(paths)
            ci_changed = ".github/workflows/ci.yml" in paths
            # CI edits come from the candidate head. Otherwise a branch made
            # before this rollout inherits CI from the current default branch.
            has_ci = self.api.has_ci(sha if ci_changed else self.default)
            if policy_changed:
                self.api.verify_gate(sha if ".github/workflows/review-gate.yml" in paths else self.default)
            if has_ci or policy_changed:
                self.api.followup(pr, "pending", "v1" + (" ci" if has_ci else "") + (" gate" if policy_changed else ""))
            # File pagination is another API boundary. Verify again before merge.
            current = self.api.call(f"repos/{self.api.repo}/pulls/{n}")
            if not unchanged(pr, current) or not eligible(current, self.default):
                return "changed"
            self.api.assert_current_epoch(sha)
            state, _ = checked_decision(self.api, current)
            if state != "success":
                return "waiting_for_valid_review"
            final = self.api.call(f"repos/{self.api.repo}/pulls/{n}")
            if not unchanged(current, final) or not eligible(final, self.default):
                return "changed"
            # The GitHub API binds this PUT to the head SHA and native branch
            # rules. Labels/custom evidence have no atomic compare-and-merge
            # API: this is the final observed snapshot, not a transactional lock
            # against edits arriving after this read. Never grant this bot bypass.
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
            self.retry_needed = True
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
            api.dispatch_retry(default, retry + 1)
        else:
            raise RuntimeError("GitHub mergeability remains unknown after bounded retries; rerun this workflow.")
    if failures:
        raise RuntimeError("Auto-merge needs retry for PRs: " + ", ".join(map(str, failures)))


if __name__ == "__main__":
    run()
