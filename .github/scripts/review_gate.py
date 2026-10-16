#!/usr/bin/env python3
"""Publish a required check from trusted review evidence, without running PR code."""
import datetime as dt
import json
import os
import re
import time
import urllib.request

CHECK_NAME = "Nimo Review Gate"
STATUS_NAME = "Nimo Review Approval"
STATUS_HISTORY_LIMIT = 900  # Leave room to replace a green status before GitHub's 1000-status cap.
ACTIONS_APP_ID = 15368
HUMANS = {"wiwiwilliam": 88575784, "leihaowen": 43492569}
CODEX = "chatgpt-codex-connector[bot]"
CODEX_USER_ID = 199175422
CLAUDE = "claude[bot]"
BOTS = {CODEX, CLAUDE}
BRIDGE = "github-actions[bot]"
BRIDGE_ID = 41898282
BRIDGE_MARKER = "<!-- nimo-review-gate-approval -->"
CODEX_RECEIPT_MARKER = "<!-- codex-review-receipt:v1 -->"
TICK = chr(96)


class SupersededEvaluation(RuntimeError):
    """A newer trusted invalidation owns this commit verdict."""


class UncertainEpoch(SupersededEvaluation):
    """Timestamp precision cannot establish ownership of a reused commit."""


def stamp(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp() if value else 0


def bot(user, login=None):
    return user.get("type") == "Bot" and user.get("login") in ({login} if login else BOTS)


def gql_bot(author, login=None):
    author = author or {}
    allowed = {login} if login else BOTS
    identities = allowed | {name.removesuffix("[bot]") for name in allowed}
    return author.get("__typename") == "Bot" and author.get("login") in identities


def human(user):
    return user.get("type") == "User" and HUMANS.get(user.get("login")) == user.get("id")


def trusted_human(review):
    return human(review["user"]) or (review["user"].get("type") == "User"
        and review.get("author_association") in {"OWNER", "MEMBER", "COLLABORATOR"})


def bridge_review(review):
    user = review["user"]
    return (user.get("type") == "Bot" and user.get("login") == BRIDGE
            and user.get("id") == BRIDGE_ID and (review.get("body") or "").startswith(BRIDGE_MARKER))


def public_body(body):
    return (body or "").split("<details", 1)[0]


def verdict_text(body):
    """Public text of a Codex verdict, without the marker its receipt comments start with."""
    text = public_body(body)
    if text.startswith(CODEX_RECEIPT_MARKER):
        text = text[len(CODEX_RECEIPT_MARKER):].lstrip()
    return text


def matches(short, sha):
    return bool(re.fullmatch(r"[0-9a-f]{7,40}", short or "")) and sha.startswith(short)


def reviewed_commit(body, sha):
    found = re.search(r"\*\*Reviewed commit:\*\*\s*" + TICK + r"([0-9a-f]{7,40})" + TICK,
                      public_body(body))
    return bool(found and matches(found.group(1), sha))


def priority(body):
    levels = re.findall(r"\[P([0-3])\]|\bP([0-3])\s+Badge\b|priority[-_]P([0-3])\b|/badge/P([0-3])-",
                        public_body(body), re.I)
    return min((int(next(value for value in group if value)) for group in levels), default=None)


def opinions(reviews, sha=None):
    latest = {}
    for review in sorted(reviews, key=lambda r: (stamp(r.get("submitted_at")), r["id"])):
        if review["state"] in {"APPROVED", "CHANGES_REQUESTED", "DISMISSED"}:
            previous = latest.get(review["user"]["login"])
            # An old verdict may clear an older request, but cannot supersede a
            # request made specifically on the current commit, even if dismissed.
            if (sha and review["state"] in {"APPROVED", "DISMISSED"}
                    and review.get("commit_id") != sha and previous
                    and previous["state"] == "CHANGES_REQUESTED"
                    and previous.get("commit_id") == sha):
                continue
            latest[review["user"]["login"]] = review
    return latest


def last_request(comments, login):
    mention = "@codex" if login == CODEX else "@claude"
    return max((stamp(c.get("updated_at") or c["created_at"]) for c in comments
                if trusted_human(c)
                and re.search(re.escape(mention) + r"\s+review\b", c.get("body", ""), re.I)), default=0)


def decision(pr, reviews, comments, threads, reactions=()):
    sha = pr["head"]["sha"]
    if pr["state"] != "open" or pr.get("draft"):
        return "failure", "Pull request is closed or still a draft."
    if any(label["name"].lower() == "hold" for label in pr.get("labels", [])):
        return "failure", "The hold label prevents merging."
    latest = opinions(reviews, sha)
    requests = [r["user"]["login"] for r in latest.values()
                if r["state"] == "CHANGES_REQUESTED" and trusted_human(r)]
    if requests:
        return "failure", "Human changes requested: " + ", ".join(requests)

    # Named reviewers may accept risk, but cannot approve their own PR.
    for review in latest.values():
        if (human(review["user"]) and review["user"]["id"] != pr["user"]["id"]
                and review["state"] == "APPROVED" and review["commit_id"] == sha):
            return "success", review["user"]["login"] + " approved the current commit."

    active_bot_changes = [r for r in latest.values() if bot(r["user"])
                          and r["state"] == "CHANGES_REQUESTED" and r["commit_id"] == sha]
    if active_bot_changes:
        return "failure", "A trusted reviewer requested changes on the current commit."

    # Resolving a conversation does not overturn a blocking review verdict.
    # A new commit must be reviewed, or a named maintainer must explicitly approve.
    for thread in threads:
        author = thread["author"] or {}
        if (gql_bot(author)
                and thread.get("commit_id") == sha
                and priority(thread["body"]) in {0, 1}):
            return "failure", "A P0/P1 finding on the current commit prevents robot approval."

    native_reviews = [r for r in reviews if bot(r["user"], CODEX) and r["commit_id"] == sha
                      and r["state"] != "DISMISSED"]
    native_comments = [c for c in comments if bot(c["user"], CODEX)
                       and c.get("verified_commit_id") == sha
                       and reviewed_commit(c.get("body"), sha)]
    blocking_reviews = [r for r in reviews if bot(r["user"]) and r["commit_id"] == sha
                        and r["state"] != "DISMISSED"]
    if any(priority(r.get("body")) in {0, 1} for r in blocking_reviews + native_comments):
        return "failure", "Codex/Claude reported a P0/P1 finding on the current commit."

    for review in latest.values():
        if (bot(review["user"], CLAUDE) and review["state"] == "APPROVED"
                and review["commit_id"] == sha
                and stamp(review.get("submitted_at")) > last_request(comments, CLAUDE)):
            return "success", "Claude approved the current commit."

    if pr["user"].get("login") == BRIDGE:
        return "pending", "Actions-authored PRs require a named human approval or a formal Claude approval."

    summaries = [c for c in comments if bot(c["user"], CODEX)
                 and c.get("body", "").startswith("<!-- codex-pull-request-review-summary -->")]
    if not summaries:
        return "pending", "Awaiting a Codex review or a named human approval."
    summary = max(summaries, key=lambda c: (stamp(c.get("updated_at", c["created_at"])), c["id"]))
    row = next((line for line in public_body(summary["body"]).splitlines()
                if re.match(r"^\|\s*.*\*\*Code Review\*\*\s*\|", line)), "")
    columns = row.split("|")
    if len(columns) < 5:
        return "pending", "Codex summary format is unrecognized; approval is required."
    short = re.search(TICK + r"([0-9a-f]{7,40})" + TICK, columns[3])
    if not short or summary.get("verified_commit_id") != sha or not matches(short.group(1), sha):
        return "pending", "Codex has not reviewed the current commit."
    if "**Completed**" not in columns[2]:
        return "pending", "Codex review is running, failed, or otherwise incomplete."

    candidates = []
    for item in native_comments + native_reviews:
        body = verdict_text(item.get("body"))
        if body.startswith("Codex Review:") or body.lstrip().startswith("### 💡 Codex Review"):
            candidates.append((stamp(item.get("submitted_at") or item.get("created_at")), item["id"], body))
    if not candidates:
        completion = re.search(r'datetime="([^"]+)"', columns[2])
        finished_at = stamp(completion.group(1)) if completion else 0
        current_threads = [t for t in threads if gql_bot(t["author"], CODEX)
                           and t.get("commit_id") == sha]
        # Automatic native reviews may publish only a reaction on the PR body.
        # The reactions API labels this bot as User; verify its immutable ID.
        # A stale reaction cannot attest to a newer commit or a later re-review.
        matching = [r for r in reactions if r["user"].get("login") == CODEX
                    and r["user"].get("id") == CODEX_USER_ID and r["content"] == "+1"
                    and finished_at > last_request(comments, CODEX)
                    and int(finished_at) <= stamp(r["created_at"]) <= finished_at + 10
                    and stamp(r["created_at"]) > last_request(comments, CODEX)]
        if matching and not current_threads and not native_reviews:
            return "success", "Codex completed the current-commit review and published a matching no-blocking thumbs-up."
        return "pending", "Completed is not approval; awaiting an explicit Codex verdict or a matching fresh thumbs-up."
    result_at, _, result_body = max(candidates)
    # Unknown review text is incomplete evidence, not an invented P1 finding.
    # A later explicit clean review may clarify it; known P0/P1 findings above
    # still require a new commit or named-maintainer acceptance.
    if any(r.get("body", "").strip() and priority(r.get("body")) is None
           and not public_body(r.get("body")).startswith("Codex Review:")
           and not public_body(r.get("body")).lstrip().startswith("### 💡 Codex Review")
           and stamp(r.get("submitted_at")) >= result_at for r in native_reviews):
        return "pending", "Current Codex review text is unrecognized; awaiting clarification or maintainer approval."
    if result_at <= last_request(comments, CODEX):
        return "pending", "A new Codex review was requested after the previous verdict."
    # Bind a Completed summary to its verdict, not a later review of the same SHA.
    completion = re.search(r'datetime="([^"]+)"', columns[2])
    if completion and stamp(completion.group(1)) <= last_request(comments, CODEX):
        return "pending", "The completed review predates the latest review request."
    if not completion or abs(stamp(completion.group(1)) - result_at) > 10:
        return "pending", "Codex completion and verdict do not identify the same review."
    native_threads = [t for t in threads if gql_bot(t["author"], CODEX)
                      and t.get("commit_id") == sha]
    if any(priority(t["body"]) not in {2, 3} for t in native_threads):
        return "failure", "Codex has blocking or unclassified findings on the current commit."
    if result_body.startswith("Codex Review: Didn't find any major issues."):
        return "success", "Codex completed the current-commit review without blocking findings."
    nonblocking_threads = native_threads and all(priority(t["body"]) in {2, 3} for t in native_threads)
    suggestions = (result_body.startswith("Codex Review: Here are some suggestions.")
                   or result_body.lstrip().startswith("### 💡 Codex Review"))
    if suggestions and nonblocking_threads:
        return "success", "Codex completed the current-commit review with only P2/P3 suggestions."
    return "failure", "Codex did not provide a no-blocking verdict; review findings or approve manually."


class GitHub:
    def __init__(self):
        self.repo = os.environ["GITHUB_REPOSITORY"]
        self.token = os.environ["GH_TOKEN"]
        self.api = os.environ.get("GITHUB_API_URL", "https://api.github.com")
        if self.api != "https://api.github.com":
            raise ValueError("Only the configured GitHub.com API is supported.")

    def call(self, path, method="GET", payload=None):
        # No PR-provided URLs or executable text enter this client.
        req = urllib.request.Request(self.api + "/" + path, method=method,
                                     data=json.dumps(payload).encode() if payload is not None else None,
                                     headers={"Authorization": "Bearer " + self.token,
                                              "Accept": "application/vnd.github+json",
                                              "Content-Type": "application/json",
                                              "X-GitHub-Api-Version": "2022-11-28"})
        with urllib.request.urlopen(req, timeout=25) as response:
            return json.load(response)

    def pages(self, path, key=None):
        results = []
        for page in range(1, 101):
            data = self.call(path + ("&" if "?" in path else "?") + "per_page=100&page=" + str(page))
            rows = data[key] if key else data
            results.extend(rows)
            if len(rows) < 100:
                return results
        raise RuntimeError("Pagination exceeded the safe bound; gate remains blocked.")

    def status_history(self, sha):
        return [s for s in self.pages(f"repos/{self.repo}/commits/{sha}/statuses")
                if s.get("context", "").lower() == STATUS_NAME.lower()]

    @staticmethod
    def epoch_marker(history):
        trusted = [s for s in history if s.get("creator", {}).get("id") == BRIDGE_ID
                   and s.get("creator", {}).get("login") == BRIDGE]
        # Compensation only revokes its own generation. It must not republish
        # a cached competing marker: another invalidation may already own SHA.
        revoked = {match.group(1) for s in trusted if (match := re.match(
            r"^Review superseded epoch ([0-9]+\.[0-9]+):", s.get("description") or ""))}
        return next((s for s in trusted if (match := re.match(
            r"^Review epoch ([0-9]+\.[0-9]+):", s.get("description") or ""))
            and match.group(1) not in revoked), None)

    def assert_current_epoch(self, sha, history=None, initialize=False):
        epoch = os.environ.get("NIMO_GATE_EPOCH")
        if not epoch:
            return
        marker = self.epoch_marker(history if history is not None else self.status_history(sha))
        if marker is None:
            if initialize:
                return
            raise RuntimeError("The trusted event generation has not been initialized.")
        actual = re.match(r"^Review epoch ([0-9]+\.[0-9]+):", marker["description"]).group(1)
        if actual != epoch:
            if initialize:
                # Run IDs do not order invalidations: rerunning an old run can
                # publish a newer marker. Only take over markers definitely
                # published before the trusted bootstrap began its snapshot.
                started = stamp(os.environ.get("NIMO_GATE_BOOTSTRAP_STARTED_AT"))
                created = stamp(marker.get("created_at"))
                if started and created and created < int(started):
                    return
                if not started or not created or int(created) == int(started):
                    raise UncertainEpoch("Cannot order the existing marker against this bootstrap.")
            raise SupersededEvaluation("Another event owns this commit verdict.")

    def status(self, sha, state, initialize=False):
        history = self.status_history(sha)
        try:
            self.assert_current_epoch(sha, history, initialize=initialize)
        except UncertainEpoch:
            # Do not claim the other epoch or leave an ambiguous old green.
            # A description without a marker preserves the authoritative
            # generation in history, including invalidations racing this POST.
            latest = history[0] if history else None
            if (not latest or latest.get("state") == "success"
                    or latest.get("creator", {}).get("id") != BRIDGE_ID
                    or latest.get("creator", {}).get("login") != BRIDGE):
                blocked = self.call(f"repos/{self.repo}/statuses/{sha}", "POST", {
                    "context": STATUS_NAME, "state": "pending",
                    "description": "Epoch order uncertain; awaiting a fresh trusted event."})
                if (blocked.get("creator", {}).get("id") != BRIDGE_ID
                        or blocked.get("creator", {}).get("login") != BRIDGE
                        or blocked.get("state") != "pending" or blocked.get("context") != STATUS_NAME):
                    raise RuntimeError("Ambiguous epoch could not be blocked.")
            raise
        latest = history[0] if history else None
        exhausted = len(history) >= STATUS_HISTORY_LIMIT
        requested = "failure" if exhausted else state
        if (latest and latest.get("state") == requested
                and (not initialize or not os.environ.get("NIMO_GATE_EPOCH")
                     or (self.epoch_marker(history) and self.epoch_marker(history)["description"].startswith(
                         "Review epoch " + os.environ["NIMO_GATE_EPOCH"] + ":")))
                and latest.get("creator", {}).get("login") == BRIDGE
                and latest.get("creator", {}).get("id") == BRIDGE_ID):
            if exhausted:
                raise RuntimeError("Status history is near GitHub's limit; push a new commit before reviewing again.")
            return
        state = requested
        description = {"pending": "Awaiting current-commit trusted review",
                       "success": "Current-commit review policy passed",
                       "failure": "Current-commit review policy prevents merging"}[state]
        if initialize and os.environ.get("NIMO_GATE_EPOCH"):
            description = "Review epoch " + os.environ["NIMO_GATE_EPOCH"] + ": initializing new head"
        published = self.call(f"repos/{self.repo}/statuses/{sha}", "POST", {
            "context": STATUS_NAME, "state": state, "description": description,
            "target_url": os.environ["GITHUB_SERVER_URL"] + "/" + self.repo
                + "/actions/runs/" + os.environ["GITHUB_RUN_ID"]})
        creator = published.get("creator", {})
        if (creator.get("login") != BRIDGE or creator.get("id") != BRIDGE_ID
                or published.get("state") != state or published.get("context") != STATUS_NAME):
            raise RuntimeError("Commit status publisher or verdict did not match the gate.")
        if os.environ.get("NIMO_GATE_EPOCH"):
            try:
                after = self.status_history(sha)
                if initialize:
                    # Our initialization marker must not hide an invalidation
                    # published between the preflight history read and POST.
                    before = {(s.get("id"), s.get("description"), s.get("created_at")) for s in history}
                    for candidate in after:
                        marker = self.epoch_marker([candidate])
                        if (marker and not marker["description"].startswith("Review epoch " + os.environ["NIMO_GATE_EPOCH"] + ":")
                                and (marker.get("id"), marker.get("description"), marker.get("created_at")) not in before):
                            raise SupersededEvaluation("Another invalidation raced head initialization.")
                self.assert_current_epoch(sha, after)
            except SupersededEvaluation:
                # Revoke our own marker, including an initialization that hid
                # the competing marker. Never reassert a cached other epoch:
                # a third invalidation may have arrived after the history read.
                revoked = self.call(f"repos/{self.repo}/statuses/{sha}", "POST", {
                    "context": STATUS_NAME, "state": "pending",
                    "description": "Review superseded epoch " + os.environ["NIMO_GATE_EPOCH"] + ": newer invalidation owns this head."})
                if (revoked.get("creator", {}).get("id") != BRIDGE_ID
                        or revoked.get("creator", {}).get("login") != BRIDGE
                        or revoked.get("state") != "pending" or revoked.get("context") != STATUS_NAME):
                    raise RuntimeError("Superseded generation could not be revoked.")
                raise
            except Exception:
                # A green write is never accepted without the post-write check.
                self.call(f"repos/{self.repo}/statuses/{sha}", "POST", {
                    "context": STATUS_NAME, "state": "failure",
                    "description": "Event-generation verification failed after publication."})
                raise
        if exhausted:
            raise RuntimeError("Status history is near GitHub's limit; push a new commit before reviewing again.")

    def threads(self, number):
        owner, name = self.repo.split("/")
        query = """query($owner:String!,$name:String!,$number:Int!,$cursor:String){
          repository(owner:$owner,name:$name){pullRequest(number:$number){
            reviewThreads(first:100,after:$cursor){
              pageInfo{hasNextPage endCursor} nodes{comments(first:1){nodes{body author{login __typename} pullRequestReview{commit{oid}}}}
              }
            }
          }}
        }"""
        cursor, result = None, []
        for _ in range(100):
            data = self.call("graphql", "POST", {"query": query, "variables": {
                "owner": owner, "name": name, "number": number, "cursor": cursor}})
            if data.get("errors"):
                raise RuntimeError("Review thread query failed; gate remains blocked.")
            collection = data["data"]["repository"]["pullRequest"]["reviewThreads"]
            for thread in collection["nodes"]:
                first = thread["comments"]["nodes"]
                if not first:
                    raise RuntimeError("Review thread has no accessible root comment.")
                root_comment = first[0]
                review_commit = (root_comment.get("pullRequestReview") or {}).get("commit") or {}
                result.append({"body": root_comment["body"], "author": root_comment["author"],
                               "commit_id": review_commit.get("oid")})
            if not collection["pageInfo"]["hasNextPage"]:
                return result
            cursor = collection["pageInfo"]["endCursor"]
        raise RuntimeError("Thread pagination exceeded the safe bound.")

    def bind_comments(self, comments, sha):
        # Native clean verdicts expose only abbreviated hashes. Resolve them
        # through GitHub and require the exact full SHA; ambiguous prefixes fail.
        resolved = {}
        for comment in comments:
            if not bot(comment["user"], CODEX):
                continue
            body = public_body(comment.get("body"))
            if body.startswith("<!-- codex-pull-request-review-summary -->"):
                row = next((line for line in body.splitlines() if "**Code Review**" in line), "")
                found = re.search(TICK + r"([0-9a-f]{7,40})" + TICK, row)
            else:
                found = re.search(r"\*\*Reviewed commit:\*\*\s*" + TICK + r"([0-9a-f]{7,40})" + TICK, body)
            if found and matches(found.group(1), sha):
                short = found.group(1)
                if short not in resolved:
                    resolved[short] = self.call(f"repos/{self.repo}/commits/{short}")["sha"]
                comment["verified_commit_id"] = resolved[short]
        return comments

    def evidence(self, number):
        head = self.call(f"repos/{self.repo}/pulls/{number}")["head"]["sha"]
        return {"reviews": self.pages(f"repos/{self.repo}/pulls/{number}/reviews"),
                "comments": self.bind_comments(self.pages(f"repos/{self.repo}/issues/{number}/comments"), head),
                "threads": self.threads(number),
                "reactions": self.pages(f"repos/{self.repo}/issues/{number}/reactions")}

    def sync_approval(self, pr, state, reason):
        """Bind the validated outcome to this PR's formal approval requirement."""
        n, sha = pr["number"], pr["head"]["sha"]
        if state == "success":
            self.assert_current_epoch(sha)
        reviews = self.pages(f"repos/{self.repo}/pulls/{n}/reviews")
        own = [r for r in reviews if bridge_review(r)]
        latest = opinions(own).get(BRIDGE)
        if state != "success":
            # Remove our approvals when their evidence is no longer valid.
            # Dismissal avoids posting an independent Request Changes that would
            # contradict the permitted named-maintainer override.
            for review in own:
                if review["state"] == "APPROVED":
                    self.call(f"repos/{self.repo}/pulls/{n}/reviews/{review['id']}/dismissals",
                              "PUT", {"message": "审查依据已失效，等待最新提交的有效审查或维护者审批。"})
            return
        current_reviews = opinions(reviews, sha)
        named_override = any(human(r["user"]) and r["state"] == "APPROVED"
                             and r.get("commit_id") == sha
                             and r["user"]["id"] != pr["user"]["id"]
                             and reason == r["user"]["login"] + " approved the current commit."
                             for r in current_reviews.values())
        # Never erase a current robot rejection that arrived after evaluation.
        # Only a verified named-maintainer approval may override that rejection.
        for review in current_reviews.values():
            if review["state"] == "CHANGES_REQUESTED" and bot(review["user"]):
                if review.get("commit_id") == sha and not named_override:
                    raise RuntimeError("A new current-commit robot rejection requires fresh evaluation.")
                self.call(f"repos/{self.repo}/pulls/{n}/reviews/{review['id']}/dismissals",
                          "PUT", {"message": "当前提交已满足机器人或指定维护者的放行规则。"})
        if latest and latest["state"] == "APPROVED" and latest["commit_id"] == sha:
            return
        # A validated formal robot approval already meets the requirement.
        if any(bot(r["user"]) and r["state"] == "APPROVED" and r["commit_id"] == sha
               for r in opinions(reviews).values()):
            return
        # Human approval already satisfies the PR-specific requirement.
        if " approved the current commit." in reason:
            return
        approved = self.call(f"repos/{self.repo}/pulls/{n}/reviews", "POST", {
            "commit_id": sha, "event": "APPROVE",
            "body": BRIDGE_MARKER + "\n已核对官方机器人的最新提交审查结果，符合无阻塞放行规则。"
                    + "\n\n提交：" + sha + "\n依据：" + reason})
        if not bridge_review(approved) or approved.get("commit_id") != sha or approved["state"] != "APPROVED":
            raise RuntimeError("Formal approval publisher or commit did not match the gate.")

    def check(self, sha):
        # The bootstrap covers its snapshot. A head first discovered afterwards
        # must also invalidate any status left from an earlier use of that SHA.
        known = os.environ.get("NIMO_GATE_BOOTSTRAP_HEADS")
        if known is not None and sha not in json.loads(known):
            self.status(sha, "pending", initialize=True)
        self.assert_current_epoch(sha)
        self.check_heads = getattr(self, "check_heads", {})
        external = "nimo-review-gate:" + sha
        try:
            existing = self.pages(f"repos/{self.repo}/commits/{sha}/check-runs?filter=all", "check_runs")
        except Exception:
            existing = []  # Create a fresh blocking check without relying on the lookup.
        own = [c for c in existing if c["name"] == CHECK_NAME
               and c.get("app", {}).get("id") == ACTIONS_APP_ID and c.get("external_id") == external]
        payload = {"name": CHECK_NAME, "status": "in_progress",
                   "started_at": dt.datetime.now(dt.timezone.utc).isoformat(),
                   "details_url": os.environ["GITHUB_SERVER_URL"] + "/" + self.repo
                   + "/actions/runs/" + os.environ["GITHUB_RUN_ID"],
                   "output": {"title": "Evaluating trusted reviews", "summary": "Review evidence is being checked."}}
        # GitHub keeps a terminal conclusion when a completed run is PATCHed.
        # Create a fresh run to block a formerly successful SHA during refresh.
        active = [c for c in own if c.get("status") in {"queued", "in_progress"}
                  and c.get("conclusion") is None]
        if active:
            check_id = max(active, key=lambda c: c["id"])["id"]
            self.call(f"repos/{self.repo}/check-runs/{check_id}", "PATCH", payload)
            self.check_heads[check_id] = sha
            return check_id
        created = self.call(f"repos/{self.repo}/check-runs", "POST",
                            {**payload, "head_sha": sha, "external_id": external})
        if created.get("app", {}).get("id") != ACTIONS_APP_ID:
            raise RuntimeError("Unexpected check publisher.")
        self.check_heads[created["id"]] = sha
        return created["id"]

    def publish(self, check_id, state, explanation):
        payload = {"status": "in_progress" if state == "pending" else "completed",
                   "output": {"title": "Merge permitted" if state == "success" else "Merge blocked",
                              "summary": explanation}}
        if state != "pending":
            payload["conclusion"] = state
            payload["completed_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
        self.call(f"repos/{self.repo}/check-runs/{check_id}", "PATCH", payload)
        # Publish a distinct commit-status context for the enforced branch rule.
        # Diagnostic checks retain their original name, avoiding name collisions.
        self.status(self.check_heads[check_id], state)


def event_allowed(name, event):
    if name == "issue_comment":
        if not event.get("issue", {}).get("pull_request"):
            return False
        comment = event["comment"]
        old_body = event.get("changes", {}).get("body", {}).get("from", "")
        return bot(comment["user"]) or (trusted_human(comment) and any(mention in body for mention in ["@codex", "@claude"]
                                         for body in [(comment.get("body") or "").lower(), old_body.lower()]))
    return True


def checked_decision(api, pr):
    initial = api.evidence(pr["number"])
    state, reason = decision(pr, initial["reviews"], initial["comments"], initial["threads"], initial.get("reactions", []))
    # Reaction publication can trail the Completed summary by a few seconds,
    # and GitHub does not expose an Actions event for adding that reaction.
    for _ in range(5):
        if state != "pending" or not reason.startswith("Completed is not approval;"):
            break
        time.sleep(2)
        initial = api.evidence(pr["number"])
        state, reason = decision(pr, initial["reviews"], initial["comments"], initial["threads"], initial.get("reactions", []))
    if state == "success":
        final = api.evidence(pr["number"])
        final_state, final_reason = decision(pr, final["reviews"], final["comments"], final["threads"], final.get("reactions", []))
        # Ordinary comments and unrelated reactions do not alter review approval.
        if (final_state, final_reason) != (state, reason):
            return "pending", "Review decision changed during evaluation; awaiting a fresh check."
        state, reason = final_state, final_reason
    return state, reason


def run():
    with open(os.environ["GITHUB_EVENT_PATH"], encoding="utf-8") as event_file:
        event = json.load(event_file)
    name = os.environ["GITHUB_EVENT_NAME"]
    if not event_allowed(name, event):
        print("No trusted review evidence changed.")
        return
    api = GitHub()
    default = event["repository"]["default_branch"]
    pulls = [p for p in api.pages(f"repos/{api.repo}/pulls?state=open") if p["base"]["ref"] == default]
    # A PR-specific publisher must not overwrite a different PR's newer
    # invalidation. Only policy updates/manual global refreshes evaluate all.
    scoped_numbers = set(json.loads(os.environ.get("NIMO_GATE_BOOTSTRAP_NUMBERS", "[]")))
    if scoped_numbers:
        scoped_heads = {p["head"]["sha"] for p in pulls if p["number"] in scoped_numbers}
        scoped_heads.update(json.loads(os.environ.get("NIMO_GATE_BOOTSTRAP_AFFECTED_HEADS", "[]")))
        pulls = [p for p in pulls if p["head"]["sha"] in scoped_heads]
    # Refresh every PR so a later queued run also catches evidence changes
    # for PRs already processed by an earlier run.
    groups = {}
    for pr in pulls:
        groups.setdefault(pr["head"]["sha"], []).append(pr)
    trigger = event.get("pull_request", {}).get("number") or event.get("issue", {}).get("number")
    # Put the directly changed PR first, while still refreshing all PRs.
    ordered = sorted(groups.items(), key=lambda group: not any(p["number"] == trigger for p in group[1]))
    errors, prepared = [], []
    bootstrap_failures = set(json.loads(os.environ.get("NIMO_GATE_BOOTSTRAP_FAILURES", "[]")))
    # Invalidate every previous green check before expensive review lookups.
    # A timeout during evaluation then leaves unprocessed PRs blocked.
    for sha, cohort in ordered:
        if sha in bootstrap_failures:
            errors.append(sha)
            print(sha[:12], "bootstrap failed; this commit group remains blocked.")
            continue
        try:
            prepared.append((sha, cohort, api.check(sha)))
        except SupersededEvaluation:
            print(sha[:12], "superseded event skipped; newer invalidation remains authoritative.")
            continue
        except Exception as error:
            errors.append(sha)
            for item in cohort:
                try:
                    api.sync_approval(item, "failure", "Unable to establish a blocking check.")
                except Exception:
                    pass
            print(sha[:12], "check initialization failed:", type(error).__name__)
    for sha, cohort, check_id in prepared:
        try:
            outcomes = []
            for item in cohort:
                n = item["number"]
                pr = api.call(f"repos/{api.repo}/pulls/{n}")
                if pr["head"]["sha"] != sha:
                    raise RuntimeError("PR head changed while evaluating; re-run the gate.")
                state, reason = checked_decision(api, pr)
                current = api.call(f"repos/{api.repo}/pulls/{n}")
                if (current["head"]["sha"] != sha or current["state"] != "open"
                        or current["base"]["ref"] != default
                        or current["updated_at"] != pr["updated_at"]
                        or current["draft"] != pr["draft"]
                        or sorted(l["name"] for l in current.get("labels", [])) != sorted(l["name"] for l in pr.get("labels", []))):
                    raise RuntimeError("PR state changed while evaluating; re-run the gate.")
                api.sync_approval(current, state, reason)
                outcomes.append((state, "#" + str(n) + ": " + reason))
            # A SHA shared by multiple PRs must meet the policy for all of them.
            state = ("failure" if any(s == "failure" for s, _ in outcomes) else
                     "pending" if any(s == "pending" for s, _ in outcomes) else "success")
            if state == "success":
                final_outcomes = []
                for item in cohort:
                    current = api.call(f"repos/{api.repo}/pulls/{item['number']}")
                    if (current["head"]["sha"] != sha or current["state"] != "open"
                            or current["base"]["ref"] != default):
                        raise RuntimeError("PR changed before publishing the gate.")
                    final_state, final_reason = checked_decision(api, current)
                    after = api.call(f"repos/{api.repo}/pulls/{item['number']}")
                    if (after["head"]["sha"] != sha or after["state"] != "open"
                            or after["base"]["ref"] != default or after["draft"] != current["draft"]
                            or sorted(l["name"] for l in after.get("labels", [])) != sorted(l["name"] for l in current.get("labels", []))):
                        final_state, final_reason = "failure", "PR metadata changed during final review verification."
                    elif after["updated_at"] != current["updated_at"]:
                        # A review may arrive after the last evidence read. Re-evaluate
                        # changed generations without treating ordinary comments as blockers.
                        final_state, final_reason = checked_decision(api, after)
                        settled = api.call(f"repos/{api.repo}/pulls/{item['number']}")
                        if (settled["head"]["sha"] != sha or settled["state"] != "open"
                                or settled["base"]["ref"] != default
                                or settled["draft"] != after["draft"]
                                or sorted(l["name"] for l in settled.get("labels", []))
                                   != sorted(l["name"] for l in after.get("labels", []))):
                            final_state, final_reason = "failure", "PR metadata changed during final review verification."
                        elif settled["updated_at"] != after["updated_at"]:
                            final_state, final_reason = "pending", "PR changed again during final verification; awaiting a fresh run."
                    if final_state != "success":
                        api.sync_approval(current, final_state, final_reason)
                    final_outcomes.append((final_state, "#" + str(item["number"]) + ": " + final_reason))
                outcomes = final_outcomes
                state = ("failure" if any(s == "failure" for s, _ in outcomes) else
                         "pending" if any(s == "pending" for s, _ in outcomes) else "success")
            reason = "\n\n".join(text for _, text in outcomes)
            api.publish(check_id, state, reason + "\n\nReviewed head: " + sha)
            print(sha[:12], state, reason)
        except SupersededEvaluation:
            print(sha[:12], "superseded event skipped; newer invalidation remains authoritative.")
            continue
        except Exception as error:
            errors.append(sha)
            for item in cohort:
                try:
                    api.sync_approval(item, "failure", "Review verification failed.")
                except Exception:
                    pass
            if check_id is not None:
                try:
                    api.publish(check_id, "failure", "Review evidence could not be verified. Re-run the gate or contact a maintainer.")
                except Exception:
                    pass
            print(sha[:12], "verification failed:", type(error).__name__)
    if errors:
        raise RuntimeError("Review verification failed for " + str(len(errors)) + " commit groups.")


if __name__ == "__main__":
    run()

