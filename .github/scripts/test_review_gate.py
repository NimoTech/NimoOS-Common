import copy
import unittest
from review_gate import CODEX, CLAUDE, HUMANS, CODEX_USER_ID, GitHub, decision, event_allowed

SHA = "abcdef1234567890abcdef1234567890abcdef12"
TIME = "2026-10-08T10:00:00Z"
TICK = chr(96)


def pr(author="other"):
    return {"number": 1, "state": "open", "draft": False, "labels": [],
            "head": {"sha": SHA}, "user": {"login": author, "id": HUMANS.get(author, 100)},
            "base": {"ref": "main"}}


def user(login):
    return {"login": login, "id": HUMANS.get(login, CODEX_USER_ID if login == CODEX else 200),
            "type": "Bot" if login in {CODEX, CLAUDE} else "User"}


def review(login, state="APPROVED", sha=SHA, id=1):
    return {"id": id, "state": state, "commit_id": sha, "submitted_at": TIME,
            "user": user(login), "author_association": "COLLABORATOR", "body": ""}


def clean():
    summary = "<!-- codex-pull-request-review-summary -->\n| Code | Other |\n"
    summary += '| 📝 **Code Review** | ✅ **Completed** <relative-time datetime="' + TIME + '">' + TIME + "</relative-time> | " + TICK + SHA[:7] + TICK + " | Manual request |\n"
    summary += "<details>No findings are mentioned in this generic footer.</details>"
    verdict = "Codex Review: Didn't find any major issues. :+1:\n\n**Reviewed commit:** " + TICK + SHA[:10] + TICK
    return [{"id": 1, "user": user(CODEX), "body": summary, "created_at": TIME, "updated_at": TIME, "verified_commit_id": SHA},
            {"id": 2, "user": user(CODEX), "body": verdict, "created_at": TIME, "verified_commit_id": SHA}]


def thread(level=1, resolved=False, outdated=False):
    return {"author": {"login": CODEX, "__typename": "Bot"}, "body": "[P" + str(level) + "] Finding",
            "commit_id": SHA, "isResolved": resolved, "isOutdated": outdated}


class GatePolicyTests(unittest.TestCase):
    def state(self, p=None, rs=None, cs=None, ts=None):
        return decision(p or pr(), rs or [], cs or [], ts or [])[0]

    def test_no_evidence_stays_pending(self):
        self.assertEqual(self.state(), "pending")

    def test_actual_native_clear_result_passes(self):
        self.assertEqual(self.state(cs=clean()), "success")

    def test_completed_alone_never_passes(self):
        self.assertEqual(self.state(cs=clean()[:1]), "pending")

    def test_running_summary_cannot_reuse_clear_result(self):
        cs = clean()
        cs[0]["body"] = cs[0]["body"].replace("**Completed**", "**Running**")
        self.assertEqual(self.state(cs=cs), "pending")

    def test_latest_summary_wins(self):
        cs = clean()
        new = copy.deepcopy(cs[0])
        new["id"] = 3
        new["body"] = new["body"].replace("**Completed**", "**Running**")
        cs.append(new)
        self.assertEqual(self.state(cs=cs), "pending")

    def test_another_sha_invalidates_robot_approval(self):
        p = pr()
        p["head"]["sha"] = "b" * 40
        self.assertEqual(self.state(p=p, cs=clean()), "pending")

    def test_too_short_sha_cannot_bind_verdict(self):
        cs = clean()
        cs[1]["body"] = cs[1]["body"].replace(SHA[:10], SHA[:6])
        self.assertEqual(self.state(cs=cs), "pending")

    def test_native_identity_cannot_be_spoofed_by_member(self):
        cs = clean()
        for c in cs:
            c["user"] = user("developer")
        self.assertEqual(self.state(cs=cs), "pending")

    def test_native_login_with_user_type_cannot_pass(self):
        cs = clean()
        for c in cs:
            c["user"]["type"] = "User"
        self.assertEqual(self.state(cs=cs), "pending")

    def test_review_hash_only_in_help_footer_cannot_pass(self):
        cs = clean()
        cs[1]["body"] = "Codex Review: Didn't find any major issues.\n<details>**Reviewed commit:** " + TICK + SHA + TICK + "</details>"
        self.assertEqual(self.state(cs=cs), "pending")

    def test_p0_and_p1_threads_block_native(self):
        for level in [0, 1]:
            self.assertEqual(self.state(cs=clean(), ts=[thread(level)]), "failure")

    def test_thread_resolution_does_not_clear_current_commit_blockers(self):
        self.assertEqual(self.state(cs=clean(), ts=[thread(resolved=True)]), "failure")
        self.assertEqual(self.state(cs=clean(), ts=[thread(outdated=True)]), "failure")

    def test_current_p1_review_blocks_even_if_thread_resolved(self):
        r = review(CODEX, "COMMENTED")
        r["body"] = "[P1] Current finding"
        self.assertEqual(self.state(rs=[r], cs=clean(), ts=[thread(resolved=True)]), "failure")

    def test_only_explicit_p2_suggestions_can_pass(self):
        cs = clean()
        cs[1]["body"] = cs[1]["body"].replace("Didn't find any major issues. :+1:", "Here are some suggestions.\n[P2] Nonblocking suggestion")
        self.assertEqual(self.state(cs=cs, ts=[thread(level=2)]), "success")

    def test_unclassified_suggestions_fail_closed(self):
        cs = clean()
        cs[1]["body"] = cs[1]["body"].replace("Didn't find any major issues. :+1:", "Here are some suggestions.")
        self.assertEqual(self.state(cs=cs), "failure")

    def test_wiwi_and_leihao_may_approve_others(self):
        for login in HUMANS:
            self.assertEqual(self.state(rs=[review(login)]), "success")

    def test_named_human_approval_may_override_bot_findings(self):
        self.assertEqual(self.state(rs=[review("leihaowen")], ts=[thread()]), "success")

    def test_other_member_approval_is_not_a_gate_approval(self):
        self.assertEqual(self.state(rs=[review("developer")]), "pending")

    def test_wrong_user_id_does_not_pass_named_approval(self):
        r = review("leihaowen")
        r["user"]["id"] = 9
        self.assertEqual(self.state(rs=[r]), "pending")

    def test_self_approval_cannot_pass(self):
        self.assertEqual(self.state(p=pr("wiwiwilliam"), rs=[review("wiwiwilliam")]), "pending")

    def test_stale_human_approval_cannot_pass(self):
        self.assertEqual(self.state(rs=[review("leihaowen", sha="c" * 40)]), "pending")

    def test_dismissed_human_approval_cannot_pass(self):
        self.assertEqual(self.state(rs=[review("leihaowen"), review("leihaowen", "DISMISSED", id=2)]), "pending")

    def test_comments_do_not_clear_a_request_for_changes(self):
        rs = [review("developer", "CHANGES_REQUESTED"), review("developer", "COMMENTED", id=2)]
        self.assertEqual(self.state(rs=rs, cs=clean()), "failure")

    def test_human_changes_requested_block_named_approval_too(self):
        self.assertEqual(self.state(rs=[review("wiwiwilliam"), review("developer", "CHANGES_REQUESTED")]), "failure")

    def test_request_changes_is_cleared_by_same_reviewer_approval(self):
        rs = [review("developer", "CHANGES_REQUESTED"), review("developer", "APPROVED", id=2)]
        self.assertEqual(self.state(rs=rs, cs=clean()), "success")

    def test_claude_approve_passes(self):
        self.assertEqual(self.state(rs=[review(CLAUDE)]), "success")

    def test_claude_request_changes_blocks(self):
        self.assertEqual(self.state(rs=[review(CLAUDE, "CHANGES_REQUESTED")], cs=clean()), "failure")

    def test_claude_old_approval_does_not_pass(self):
        self.assertEqual(self.state(rs=[review(CLAUDE, sha="d" * 40)]), "pending")

    def test_unknown_claude_bot_cannot_pass(self):
        r = review(CLAUDE)
        r["user"]["login"] = "claude-impostor[bot]"
        self.assertEqual(self.state(rs=[r]), "pending")

    def test_new_review_request_invalidates_old_native_verdict(self):
        cs = clean() + [{"id": 3, "user": user("developer"), "author_association": "MEMBER", "body": "@codex review",
                        "created_at": "2026-10-08T10:01:00Z"}]
        self.assertEqual(self.state(cs=cs), "pending")

    def test_later_completion_cannot_reuse_old_native_verdict(self):
        cs = clean()
        cs[0]["body"] = cs[0]["body"].replace(TIME, "2026-10-08T10:01:00Z")
        self.assertEqual(self.state(cs=cs), "pending")

    def test_hold_blocks_both_routes(self):
        p = pr()
        p["labels"] = [{"name": "hold"}]
        self.assertEqual(self.state(p=p, rs=[review("leihaowen")], cs=clean()), "failure")

    def test_draft_blocks_both_routes(self):
        p = pr()
        p["draft"] = True
        self.assertEqual(self.state(p=p, rs=[review("leihaowen")], cs=clean()), "failure")

    def test_issue_comments_on_issues_are_ignored(self):
        self.assertFalse(event_allowed("issue_comment", {"issue": {}, "comment": clean()[0]}))

    def test_arbitrary_pr_comments_are_ignored(self):
        self.assertFalse(event_allowed("issue_comment", {"issue": {"pull_request": {}},
            "comment": {"user": user("developer"), "body": "LGTM"}}))

    def test_pagination_reads_more_than_100_reviews(self):
        api = GitHub.__new__(GitHub)
        api.call = lambda path: list(range(100)) if path.endswith("&page=1") else [100]
        self.assertEqual(len(api.pages("reviews")), 101)

    def test_check_publisher_must_be_github_actions(self):
        api = GitHub.__new__(GitHub)
        api.repo = "owner/repo"
        api.pages = lambda *args: []
        api.call = lambda *args: {"id": 1, "app": {"id": 1}}
        import os
        from unittest.mock import patch
        with patch.dict(os.environ, {"GITHUB_SERVER_URL": "https://github.com", "GITHUB_RUN_ID": "1"}):
            with self.assertRaises(RuntimeError):
                api.check(SHA)





class GateRegressionTests(unittest.TestCase):
    state = GatePolicyTests.state
    def test_real_codex_p1_badge_blocks(self):
        t = thread()
        t["body"] = "**<sub>![P1 Badge](https://img.shields.io/badge/P1-orange)</sub>** Blocking finding"
        self.assertEqual(self.state(cs=clean(), ts=[t]), "failure")

    def test_old_commit_thread_does_not_override_new_clean_review(self):
        t = thread()
        t["commit_id"] = "d" * 40
        self.assertEqual(self.state(cs=clean(), ts=[t]), "success")

    def test_edited_older_request_invalidates_old_verdict(self):
        cs = clean() + [{"id": 3, "user": user("developer"), "author_association": "MEMBER", "body": "@codex review",
                        "created_at": "2026-10-08T09:00:00Z", "updated_at": "2026-10-08T10:01:00Z"}]
        self.assertEqual(self.state(cs=cs), "pending")

    def test_changed_review_evidence_cannot_publish_success(self):
        from review_gate import checked_decision
        snapshots = iter([
            {"reviews": [review("leihaowen")], "comments": [], "threads": []},
            {"reviews": [review("leihaowen"), review("developer", "CHANGES_REQUESTED")], "comments": [], "threads": []}
        ])
        class API:
            def evidence(self, number):
                return next(snapshots)
        self.assertEqual(checked_decision(API(), pr())[0], "pending")

    def test_stable_review_evidence_can_publish_success(self):
        from review_gate import checked_decision
        evidence = {"reviews": [review("leihaowen")], "comments": [], "threads": []}
        class API:
            def evidence(self, number):
                return evidence
        self.assertEqual(checked_decision(API(), pr())[0], "success")

    def test_actual_formal_codex_review_with_only_p2_findings_passes(self):
        r = review(CODEX, "COMMENTED")
        r["body"] = "\n### 💡 Codex Review\n\nHere are some automated review suggestions.\n\n**Reviewed commit:** " + TICK + SHA[:10] + TICK
        self.assertEqual(self.state(rs=[r], cs=clean()[:1], ts=[thread(level=2)]), "success")

    def test_actual_formal_codex_review_with_unknown_findings_blocks(self):
        r = review(CODEX, "COMMENTED")
        r["body"] = "\n### 💡 Codex Review\n\nHere are some automated review suggestions.\n\n**Reviewed commit:** " + TICK + SHA[:10] + TICK
        t = thread(level=2)
        t["body"] = "An unclassified finding"
        self.assertEqual(self.state(rs=[r], cs=clean()[:1], ts=[t]), "failure")



class GraphQLIdentityTests(unittest.TestCase):
    def test_real_graphql_bot_login_has_no_bot_suffix(self):
        t = thread()
        t["author"]["login"] = "chatgpt-codex-connector"
        self.assertEqual(decision(pr(), [], clean(), [t])[0], "failure")

    def test_graphql_user_cannot_spoof_bot_slug(self):
        t = thread()
        t["author"] = {"login": "chatgpt-codex-connector", "__typename": "User"}
        self.assertEqual(decision(pr(), [], clean(), [t])[0], "success")


class MixedFindingTests(unittest.TestCase):
    def test_p2_body_cannot_hide_an_unclassified_current_thread(self):
        cs = clean()
        cs[1]["body"] = cs[1]["body"].replace("Didn't find any major issues. :+1:", "Here are some suggestions.\n[P2] Nonblocking note")
        unknown = thread(level=2)
        unknown["body"] = "An unclassified finding"
        self.assertEqual(decision(pr(), [], cs, [thread(level=2), unknown])[0], "failure")


class TrustedHumanAndCommentEditTests(unittest.TestCase):
    def test_outsider_change_request_cannot_block_a_valid_review(self):
        r = review("outsider", "CHANGES_REQUESTED")
        r["author_association"] = "NONE"
        self.assertEqual(decision(pr(), [r], clean(), [])[0], "success")

    def test_removing_a_review_command_from_a_comment_refreshes_gate(self):
        event = {"issue": {"pull_request": {"url": "https://api.github.com/repos/owner/repo/pulls/1"}},
                 "comment": {"user": user("developer"), "author_association": "MEMBER", "body": "cancelled"},
                 "changes": {"body": {"from": "@codex review"}}}
        self.assertTrue(event_allowed("issue_comment", event))


class GateRefreshAndApprovalTests(unittest.TestCase):
    def test_outsider_review_request_does_not_invalidate_verdict(self):
        cs = clean() + [{"id": 3, "user": user("outsider"), "author_association": "NONE",
                        "body": "@codex review", "created_at": "2026-10-08T10:01:00Z"}]
        self.assertEqual(decision(pr(), [], cs, [])[0], "success")
        self.assertFalse(event_allowed("issue_comment", {"issue": {"pull_request": {"url": "x"}}, "comment": cs[-1]}))

    def api(self, reviews=None):
        api = GitHub.__new__(GitHub)
        api.repo = "owner/repo"
        api.pages = lambda *args: reviews or []
        self.calls = []
        def call(path, method="GET", payload=None):
            self.calls.append((path, method, payload))
            from review_gate import BRIDGE, BRIDGE_ID
            return {"id": 9, "user": {"login": BRIDGE, "type": "Bot", "id": BRIDGE_ID},
                    "body": payload.get("body", ""), "commit_id": SHA, "state": "APPROVED"}
        api.call = call
        return api

    def own(self):
        from review_gate import BRIDGE, BRIDGE_ID, BRIDGE_MARKER
        r = review("otherbot", "APPROVED")
        r.update(user={"login": BRIDGE, "type": "Bot", "id": BRIDGE_ID}, body=BRIDGE_MARKER + "\nverified")
        return r

    def test_robot_result_creates_pr_specific_approval(self):
        api = self.api()
        api.sync_approval(pr(), "success", "Codex completed the current-commit review without blocking findings.")
        self.assertEqual(self.calls[0][1], "POST")
        self.assertEqual(self.calls[0][2]["commit_id"], SHA)
        self.assertEqual(self.calls[0][2]["event"], "APPROVE")

    def test_existing_bridge_approval_is_not_duplicated(self):
        self.api([self.own()]).sync_approval(pr(), "success", "Codex completed review.")
        self.assertEqual(self.calls, [])

    def test_invalid_verdict_dismisses_bridge_approval(self):
        self.api([self.own()]).sync_approval(pr(), "pending", "Awaiting review.")
        self.assertEqual(self.calls[0][1], "PUT")
        self.assertTrue(self.calls[0][0].endswith("/dismissals"))

    def test_manual_approval_needs_no_bridge_duplicate(self):
        self.api([review("leihaowen")]).sync_approval(pr(), "success", "leihaowen approved the current commit.")
        self.assertEqual(self.calls, [])

    def test_named_approval_can_clear_machine_change_request(self):
        self.api([review(CLAUDE, "CHANGES_REQUESTED"), review("leihaowen", id=2)]).sync_approval(
            pr(), "success", "leihaowen approved the current commit.")
        self.assertTrue(self.calls[0][0].endswith("/dismissals"))
        self.assertEqual(len(self.calls), 1)

    def test_failed_cohort_does_not_skip_other_prs(self):
        import os, tempfile, json
        from unittest.mock import patch
        from review_gate import run
        a, b = pr(), pr()
        b.update(number=2, head={"sha": "b" * 40})
        for p in [a, b]:
            p["updated_at"] = TIME
        seen = []
        class API:
            repo = "owner/repo"
            def pages(self, path): return [a, b]
            def check(self, sha):
                if sha == SHA: raise RuntimeError("temporary API failure")
                return 2
            def call(self, path): return copy.deepcopy(b)
            def evidence(self, number): return {"reviews": [], "comments": [], "threads": []}
            def sync_approval(self, p, state, reason): seen.append(p["number"])
            def publish(self, *args): pass
        with tempfile.NamedTemporaryFile(mode="w") as f:
            json.dump({"repository": {"default_branch": "main"}, "pull_request": {"number": 1}}, f)
            f.flush()
            with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "pull_request_target", "GITHUB_EVENT_PATH": f.name}), patch("review_gate.GitHub", return_value=API()):
                with self.assertRaises(RuntimeError): run()
        self.assertEqual(seen, [1, 2])

    def test_pr_specific_event_refreshes_all_prs(self):
        import os, tempfile, json
        from unittest.mock import patch
        from review_gate import run
        a, b = pr(), pr()
        b.update(number=2, head={"sha": "b" * 40})
        for p in [a, b]: p["updated_at"] = TIME
        seen = []
        class API:
            repo = "owner/repo"
            def pages(self, path): return [a, b]
            def check(self, sha): return 1
            def call(self, path): return copy.deepcopy(a if path.endswith("/1") else b)
            def evidence(self, number): return {"reviews": [], "comments": [], "threads": []}
            def sync_approval(self, p, state, reason): seen.append(p["number"])
            def publish(self, *args): pass
        with tempfile.NamedTemporaryFile(mode="w") as f:
            json.dump({"repository": {"default_branch": "main"}, "pull_request": {"number": 1}}, f)
            f.flush()
            with patch.dict(os.environ, {"GITHUB_EVENT_NAME": "pull_request_target", "GITHUB_EVENT_PATH": f.name}), patch("review_gate.GitHub", return_value=API()): run()
        self.assertEqual(seen, [1, 2])


class FinalPublishSecurityTests(unittest.TestCase):
    def test_short_hash_collision_cannot_reuse_a_clear_verdict(self):
        api=GitHub.__new__(GitHub)
        api.repo='owner/repo'
        api.call=lambda path: {'sha':SHA}
        different=SHA[:10]+'f'*30
        cs=api.bind_comments(clean(),different)
        p=pr();p['head']['sha']=different
        self.assertEqual(decision(p,[],cs,[])[0],'pending')

    def test_ambiguous_github_commit_resolution_fails_closed(self):
        api=GitHub.__new__(GitHub);api.repo='owner/repo'
        def call(path):raise RuntimeError('Ambiguous commit prefix')
        api.call=call
        with self.assertRaises(RuntimeError):api.bind_comments(clean(),SHA)

    def test_failed_check_lookup_creates_a_fresh_blocking_check(self):
        import os
        from unittest.mock import patch
        api=GitHub.__new__(GitHub);api.repo='owner/repo'
        def pages(*args):raise RuntimeError('Transient lookup failure')
        api.pages=pages
        calls=[]
        def call(path,method,payload):
            calls.append((method,payload))
            return {'id':9,'app':{'id':15368}}
        api.call=call
        with patch.dict(os.environ,{'GITHUB_SERVER_URL':'https://github.com','GITHUB_RUN_ID':'1'}):
            self.assertEqual(api.check(SHA),9)
        self.assertEqual(calls[0][1]['status'],'in_progress')

    def test_blocking_review_arriving_during_bridge_prevents_success(self):
        import os,tempfile,json
        from unittest.mock import patch
        from review_gate import run
        p=pr();p['updated_at']=TIME
        writes=[]
        class API:
            repo='owner/repo'
            late=False
            def pages(self,path):return [p]
            def check(self,sha):return 1
            def call(self,path):return copy.deepcopy(p)
            def evidence(self,number):
                return {'reviews':[review('developer','CHANGES_REQUESTED')] if self.late else [],
                        'comments':clean(),'threads':[]}
            def sync_approval(self,p,state,reason):
                writes.append(('approval',state))
                self.late=True
            def publish(self,check,state,reason):writes.append(('check',state))
        with tempfile.NamedTemporaryFile(mode='w') as f:
            json.dump({'repository':{'default_branch':'main'}},f);f.flush()
            with patch.dict(os.environ,{'GITHUB_EVENT_NAME':'push','GITHUB_EVENT_PATH':f.name}),patch('review_gate.GitHub',return_value=API()):run()
        self.assertEqual(writes,[('approval','success'),('approval','failure'),('check','failure')])


class QueueAndApprovalBoundaryTests(unittest.TestCase):
    def test_same_second_native_review_request_invalidates_old_result(self):
        cs=clean()+[{'id':3,'user':user('developer'),'author_association':'MEMBER',
                     'body':'@codex review','created_at':TIME}]
        self.assertEqual(decision(pr(),[],cs,[])[0],'pending')

    def test_same_second_claude_review_request_invalidates_old_result(self):
        cs=[{'id':3,'user':user('developer'),'author_association':'MEMBER',
             'body':'@claude review','created_at':TIME}]
        self.assertEqual(decision(pr(),[review(CLAUDE)],cs,[])[0],'pending')

    def test_actions_authored_pr_cannot_receive_bridge_self_approval(self):
        from review_gate import BRIDGE
        p=pr(BRIDGE);p['user']['type']='Bot'
        self.assertEqual(decision(p,[],clean(),[])[0],'pending')
        self.assertEqual(decision(p,[review('wiwiwilliam')],clean(),[])[0],'success')
        self.assertEqual(decision(p,[review(CLAUDE)],[],[])[0],'success')

    def test_existing_formal_robot_approval_does_not_need_bridge_review(self):
        case=GateRefreshAndApprovalTests()
        case.api([review(CLAUDE)]).sync_approval(pr(),'success','Claude approved the current commit.')
        self.assertEqual(case.calls,[])

    def test_all_cohorts_are_blocked_before_review_evaluation(self):
        import os,tempfile,json
        from unittest.mock import patch
        from review_gate import run
        a,b=pr(),pr();b.update(number=2,head={'sha':'b'*40})
        for p in [a,b]:p['updated_at']=TIME
        calls=[]
        class API:
            repo='owner/repo'
            def pages(self,path):return [a,b]
            def check(self,sha):calls.append(('block',sha));return 1
            def call(self,path):return copy.deepcopy(a if path.endswith('/1') else b)
            def evidence(self,n):
                calls.append(('evidence',n));return {'reviews':[],'comments':[],'threads':[]}
            def sync_approval(self,*args):pass
            def publish(self,*args):pass
        with tempfile.NamedTemporaryFile(mode='w') as f:
            json.dump({'repository':{'default_branch':'main'},'pull_request':{'number':2}},f);f.flush()
            with patch.dict(os.environ,{'GITHUB_EVENT_NAME':'pull_request_target','GITHUB_EVENT_PATH':f.name}),patch('review_gate.GitHub',return_value=API()):run()
        self.assertEqual(calls[:2],[('block','b'*40),('block',SHA)])


class NativeAutomaticReviewTests(unittest.TestCase):
    def reaction(self, created=TIME, login=CODEX):
        return {'id':9,'user':user(login),'content':'+1','created_at':created}

    def test_current_completed_review_and_fresh_thumb_can_pass(self):
        self.assertEqual(decision(pr(),[],clean()[:1],[],[self.reaction()])[0],'success')

    def test_old_thumb_cannot_pass_a_later_completed_review(self):
        cs=clean()[:1]
        cs[0]['body']=cs[0]['body'].replace(TIME,'2026-10-08T10:01:00Z')
        self.assertEqual(decision(pr(),[],cs,[],[self.reaction()])[0],'pending')

    def test_thumb_from_another_user_does_not_pass(self):
        self.assertEqual(decision(pr(),[],clean()[:1],[],[self.reaction(login='developer')])[0],'pending')

    def test_fresh_thumb_cannot_override_blocking_or_unknown_threads(self):
        self.assertEqual(decision(pr(),[],clean()[:1],[thread()],[self.reaction()])[0],'failure')
        t=thread();t['body']='Unclassified finding'
        self.assertEqual(decision(pr(),[],clean()[:1],[t],[self.reaction()])[0],'pending')

    def test_new_request_invalidates_same_second_thumb(self):
        cs=clean()[:1]+[{'id':3,'user':user('developer'),'author_association':'MEMBER','body':'@codex review','created_at':TIME}]
        self.assertEqual(decision(pr(),[],cs,[],[self.reaction()])[0],'pending')

    def test_completed_check_is_not_reused_as_in_progress(self):
        import os
        from unittest.mock import patch
        from review_gate import CHECK_NAME
        api=GitHub.__new__(GitHub);api.repo='owner/repo'
        api.pages=lambda *args:[{'id':1,'name':CHECK_NAME,'app':{'id':15368},
                                'external_id':'nimo-review-gate:'+SHA,'status':'completed','conclusion':'success'}]
        calls=[]
        def call(path,method,payload):calls.append(method);return {'id':2,'app':{'id':15368}}
        api.call=call
        with patch.dict(os.environ,{'GITHUB_SERVER_URL':'https://github.com','GITHUB_RUN_ID':'1'}):
            self.assertEqual(api.check(SHA),2)
        self.assertEqual(calls,['POST'])


class ReactionOrderingTests(unittest.TestCase):
    def test_prior_thumb_inside_ten_second_window_cannot_approve_new_review(self):
        cs=clean()[:1];cs[0]['body']=cs[0]['body'].replace(TIME,'2026-10-08T10:00:09Z')
        reaction=NativeAutomaticReviewTests().reaction()
        self.assertEqual(decision(pr(),[],cs,[],[reaction])[0],'pending')


class ActualReactionIdentityTests(unittest.TestCase):
    def test_real_reaction_endpoint_user_type_with_canonical_bot_id_passes(self):
        reaction=NativeAutomaticReviewTests().reaction();reaction['user']['type']='User'
        self.assertEqual(decision(pr(),[],clean()[:1],[],[reaction])[0],'success')

    def test_bot_slug_with_wrong_immutable_id_cannot_pass(self):
        reaction=NativeAutomaticReviewTests().reaction();reaction['user']['id']=9
        self.assertEqual(decision(pr(),[],clean()[:1],[],[reaction])[0],'pending')


class ReviewDecisionStabilityTests(unittest.TestCase):
    def test_irrelevant_comment_does_not_invalidate_valid_approval(self):
        from review_gate import checked_decision
        before = {"reviews": [review("leihaowen")], "comments": [], "threads": [], "reactions": []}
        after = copy.deepcopy(before)
        after["comments"].append({"id": 9, "user": user("other"), "body": "Thanks", "created_at": TIME})
        after["reactions"].append({"id": 10, "user": user("other"), "content": "+1", "created_at": TIME})
        snapshots = iter([before, after])
        class API:
            def evidence(self, number): return next(snapshots)
        self.assertEqual(checked_decision(API(), pr())[0], "success")

    def test_reaction_at_end_of_valid_window_is_polled(self):
        from review_gate import checked_decision
        from unittest.mock import patch
        empty = {"reviews": [], "comments": clean()[:1], "threads": [], "reactions": []}
        ready = copy.deepcopy(empty)
        ready["reactions"] = [NativeAutomaticReviewTests().reaction("2026-10-08T10:00:10Z")]
        snapshots = iter([empty] * 5 + [ready, ready])
        class API:
            def evidence(self, number): return next(snapshots)
        with patch("review_gate.time.sleep") as sleep:
            self.assertEqual(checked_decision(API(), pr())[0], "success")
        self.assertEqual(sleep.call_count, 5)


class WorkflowBootstrapTests(unittest.TestCase):
    def bootstrap(self, pulls, fail_sha=None, late_approval=False, event=None, statuses=None, history_failure=False, native_approval=False):
        import os, io, json, pathlib, tempfile, textwrap
        from unittest.mock import patch
        # The bootstrap is a trusted inline workflow program, executed before checkout.
        script_dir = pathlib.Path(__file__).resolve().parent
        candidates = [script_dir / "review-gate.yml", script_dir.parent / "workflows" / "review-gate.yml"]
        workflow = next(p for p in candidates if p.exists()).read_text()
        script = textwrap.dedent(workflow.split("python3 - <<'PYTHON'\n", 1)[1].split("\n          PYTHON", 1)[0])
        calls = []
        def urlopen(req, timeout):
            if req.method == "GET":
                if "/statuses?" in req.full_url:
                    if history_failure: raise RuntimeError("Status history temporarily unavailable")
                    from urllib.parse import urlparse, parse_qs
                    page = int(parse_qs(urlparse(req.full_url).query)["page"][0])
                    history = statuses or []
                    return io.BytesIO(json.dumps(history[(page-1)*100:page*100]).encode())
                if "/reviews?" in req.full_url:
                    managed = {"id": 7, "user": {"id": 41898282, "login": "github-actions[bot]"}, "state": "APPROVED", "body": "<!-- nimo-review-gate-approval -->"}
                    human = {"id": 8, "user": user("leihaowen"), "state": "APPROVED", "body": "LGTM"}
                    if late_approval and req.full_url.endswith("&page=1"):
                        return io.BytesIO(json.dumps([human] * 100).encode())
                    items = [managed, human]
                    if native_approval: items.append(review(CLAUDE, id=9))
                    return io.BytesIO(json.dumps(items).encode())
                if "/check-runs?" in req.full_url:
                    return io.BytesIO(json.dumps({"check_runs": [{"id": 1, "name": "Nimo Review Gate", "app": {"id": 15368}, "external_id": "nimo-review-gate:" + (fail_sha or SHA)}]}).encode())
                return io.BytesIO(json.dumps(pulls).encode())
            payload = json.loads(req.data)
            calls.append(payload)
            if req.method == "PUT" and "/dismissals" in req.full_url:
                return io.BytesIO(json.dumps({"state": "DISMISSED"}).encode())
            if req.method == "POST" and "/statuses/" in req.full_url:
                return io.BytesIO(json.dumps({"state": payload["state"], "context": payload["context"], "creator": {"id": 41898282, "login": "github-actions[bot]"}}).encode())
            if req.method == "POST" and payload["head_sha"] == fail_sha: raise RuntimeError("Check unavailable")
            return io.BytesIO(json.dumps({"app": {"id": 15368}}).encode())
        with tempfile.NamedTemporaryFile(mode="w") as f, tempfile.NamedTemporaryFile(mode="w+") as env_file, tempfile.NamedTemporaryFile(mode="w+") as output_file:
            json.dump(event or {"repository": {"default_branch": "main"}}, f);f.flush()
            env = {"GITHUB_REPOSITORY": "owner/repo", "GITHUB_EVENT_PATH": f.name, "GH_TOKEN": "test", "GITHUB_ENV": env_file.name, "GITHUB_OUTPUT": output_file.name}
            with patch.dict(os.environ, env), patch("urllib.request.urlopen", side_effect=urlopen):
                error = None
                try: exec(compile(script, "trusted-bootstrap", "exec"), {})
                except RuntimeError as caught: error = caught
            env_file.seek(0)
            self.bootstrap_environment = dict(line.rstrip().split("=", 1) for line in env_file)
            output_file.seek(0)
            self.bootstrap_outputs = dict(line.rstrip().split("=", 1) for line in output_file)
        return calls, error

    def test_before_checkout_bootstrap_blocks_all_default_branch_heads(self):
        pulls = [pr(), pr(), pr()]
        pulls[1]["head"]["sha"] = "b" * 40
        pulls[2]["base"]["ref"] = "feature"
        calls, error = self.bootstrap(pulls)
        self.assertIsNone(error)
        checks = [c for c in calls if "head_sha" in c]
        self.assertEqual({c["head_sha"] for c in checks}, {SHA, "b" * 40})
        self.assertTrue(all(c["status"] == "in_progress" for c in checks))
        self.assertEqual(sum(c.get("state") == "pending" for c in calls), 2)
        self.assertTrue(all("conclusion" not in c for c in calls))

    def test_bootstrap_failure_does_not_skip_other_heads(self):
        pulls = [pr(), pr()];pulls[1]["head"]["sha"] = "b" * 40
        calls, error = self.bootstrap(pulls, fail_sha=SHA)
        self.assertIsNone(error)
        self.assertEqual({c["head_sha"] for c in calls if "head_sha" in c}, {SHA, "b" * 40})
        self.assertTrue(any(c.get("conclusion") == "failure" for c in calls))


class LatestCommitPolicyTests(unittest.TestCase):
    def test_trusted_non_review_mention_is_refreshed_after_bootstrap(self):
        event = {"issue": {"pull_request": {"url": "test"}}, "comment": {"user": user("developer"), "author_association": "MEMBER", "body": "@codex please explain"}}
        self.assertTrue(event_allowed("issue_comment", event))
        event["comment"]["author_association"] = "NONE"
        self.assertFalse(event_allowed("issue_comment", event))

    def test_clean_verdict_cannot_override_unclassified_current_thread(self):
        t=thread();t["body"]="Unclassified finding"
        self.assertEqual(decision(pr(),[],clean(),[t])[0],"failure")

    def test_late_old_commit_approval_cannot_clear_current_changes_requested(self):
        for login in ["developer", CLAUDE]:
            reviews=[review(login,"CHANGES_REQUESTED"),review(login,sha="d"*40,id=2)]
            self.assertEqual(decision(pr(),reviews,clean(),[])[0],"failure")

    def test_delayed_thumb_cannot_approve_a_later_review_request(self):
        cs=clean()[:1]+[{"id":3,"user":user("developer"),"author_association":"MEMBER","body":"@codex review","created_at":"2026-10-08T10:00:01Z"}]
        reaction=NativeAutomaticReviewTests().reaction("2026-10-08T10:00:02Z")
        self.assertEqual(decision(pr(),[],cs,[],[reaction])[0],"pending")


class CommitStatusPublisherTests(unittest.TestCase):
    def test_commit_status_binds_verdict_to_exact_head_and_distinct_context(self):
        from review_gate import STATUS_NAME, BRIDGE, BRIDGE_ID
        import os
        from unittest.mock import patch
        api = GitHub.__new__(GitHub);api.repo = "owner/repo"
        calls = []
        def call(path, method="GET", payload=None):
            if method == "GET": return []
            calls.append((path, method, payload))
            return {"context": payload["context"], "state": payload["state"], "creator": {"login": BRIDGE, "id": BRIDGE_ID}}
        api.call = call
        with patch.dict(os.environ, {"GITHUB_SERVER_URL": "https://github.com", "GITHUB_RUN_ID": "1"}):
            api.status(SHA, "success")
        self.assertEqual(calls[0][0], "repos/owner/repo/statuses/" + SHA)
        self.assertEqual(calls[0][2]["context"], STATUS_NAME)
        self.assertNotEqual(STATUS_NAME, "Nimo Review Gate")

    def test_another_actor_cannot_publish_an_accepted_status(self):
        import os
        from unittest.mock import patch
        api = GitHub.__new__(GitHub);api.repo = "owner/repo"
        api.call = lambda path, method="GET", payload=None: ([] if method == "GET" else {"context": payload["context"], "state": payload["state"], "creator": {"login": "other", "id": 1}})
        with patch.dict(os.environ, {"GITHUB_SERVER_URL": "https://github.com", "GITHUB_RUN_ID": "1"}):
            with self.assertRaises(RuntimeError): api.status(SHA, "success")


class BootstrapApprovalFailureTests(unittest.TestCase):
    def test_failed_initialization_revokes_only_managed_bridge_approval(self):
        case = WorkflowBootstrapTests()
        calls, error = case.bootstrap([pr()], fail_sha=SHA)
        self.assertIsNone(error)
        dismissals = [c for c in calls if "message" in c]
        self.assertEqual(len(dismissals), 1)
        self.assertIn("重新核对", dismissals[0]["message"])


class BootstrapLateApprovalTests(unittest.TestCase):
    def test_failed_initialization_revokes_managed_approval_after_first_page(self):
        calls, error = WorkflowBootstrapTests().bootstrap([pr()], fail_sha=SHA, late_approval=True)
        self.assertIsNone(error)
        dismissals = [c for c in calls if "message" in c]
        self.assertEqual(len(dismissals), 1)
        self.assertIn("重新核对", dismissals[0]["message"])


class FinalReviewGenerationTests(unittest.TestCase):
    def exercise(self, blocking):
        import os, tempfile, json
        from unittest.mock import patch
        from review_gate import run
        p=pr();p["updated_at"]=TIME
        writes=[]
        class API:
            repo="owner/repo"
            reads=0
            late=False
            def pages(self,path):return [p]
            def check(self,sha):return 1
            def call(self,path):
                self.reads += 1
                result=copy.deepcopy(p)
                if self.reads >= 4:
                    self.late=True
                    result["updated_at"]="2026-10-08T10:00:01Z"
                return result
            def evidence(self,n):
                return {"reviews":[],"comments":clean(),"threads":[thread()] if self.late and blocking else []}
            def sync_approval(self,p,state,reason):writes.append(("approval",state))
            def publish(self,check,state,reason):writes.append(("status",state))
        with tempfile.NamedTemporaryFile(mode="w") as f:
            json.dump({"repository":{"default_branch":"main"}},f);f.flush()
            with patch.dict(os.environ,{"GITHUB_EVENT_NAME":"push","GITHUB_EVENT_PATH":f.name}),patch("review_gate.GitHub",return_value=API()):run()
        return writes

    def test_p1_arriving_after_final_evidence_read_revokes_approval_and_blocks_status(self):
        self.assertEqual(self.exercise(True),[("approval","success"),("approval","failure"),("status","failure")])

    def test_ordinary_metadata_update_does_not_block_a_clean_review(self):
        self.assertEqual(self.exercise(False),[("approval","success"),("status","success")])

    def test_trusted_mixed_case_mention_matches_workflow_filter(self):
        event={"issue":{"pull_request":{"url":"test"}},"comment":{"user":user("developer"),"author_association":"MEMBER","body":"@Codex please explain"}}
        self.assertTrue(event_allowed("issue_comment",event))


class FinalVerdictRegressionTests(unittest.TestCase):
    def test_dismissed_old_approval_cannot_erase_current_change_request(self):
        for login in ["developer", CLAUDE]:
            reviews = [review(login, "CHANGES_REQUESTED"),
                       review(login, "DISMISSED", sha="d" * 40, id=2)]
            self.assertEqual(decision(pr(), reviews, clean(), [])[0], "failure")

    def test_approval_on_later_commit_does_not_resurrect_an_old_request(self):
        reviews = [review("developer", "CHANGES_REQUESTED", sha="a" * 40),
                   review("developer", "APPROVED", sha="b" * 40, id=2),
                   review("leihaowen", id=3)]
        self.assertEqual(decision(pr(), reviews, clean(), [])[0], "success")

    def test_robot_acceptance_never_dismisses_new_current_robot_rejection(self):
        case = GateRefreshAndApprovalTests()
        api = case.api([review(CLAUDE, "CHANGES_REQUESTED")])
        with self.assertRaises(RuntimeError):
            api.sync_approval(pr(), "success", "Codex completed the current-commit review without blocking findings.")
        self.assertEqual(case.calls, [])

    def test_override_reason_alone_cannot_dismiss_current_robot_rejection(self):
        case = GateRefreshAndApprovalTests()
        api = case.api([review(CLAUDE, "CHANGES_REQUESTED")])
        with self.assertRaises(RuntimeError):
            api.sync_approval(pr(), "success", "leihaowen approved the current commit.")
        self.assertEqual(case.calls, [])


class StatusHistoryRegressionTests(unittest.TestCase):
    def test_repeated_same_verdict_does_not_append_statuses(self):
        from review_gate import BRIDGE, BRIDGE_ID, STATUS_NAME
        api = GitHub.__new__(GitHub); api.repo = "owner/repo"
        current = {"context": STATUS_NAME, "state": "success", "creator": {"login": BRIDGE, "id": BRIDGE_ID}}
        api.pages = lambda path: [current]
        api.call = lambda *args: self.fail("Unchanged verdict must not append status")
        for _ in range(1200): api.status(SHA, "success")

    def test_near_quota_green_is_replaced_by_failure_before_limit(self):
        import os
        from unittest.mock import patch
        from review_gate import BRIDGE, BRIDGE_ID, STATUS_NAME, STATUS_HISTORY_LIMIT
        api = GitHub.__new__(GitHub); api.repo = "owner/repo"
        api.pages = lambda path: [{"context": STATUS_NAME, "state": "success", "creator": {"login": BRIDGE, "id": BRIDGE_ID}}] * STATUS_HISTORY_LIMIT
        writes = []
        def call(path, method, payload):
            writes.append(payload)
            return {**payload, "creator": {"login": BRIDGE, "id": BRIDGE_ID}}
        api.call = call
        with patch.dict(os.environ, {"GITHUB_SERVER_URL": "https://github.com", "GITHUB_RUN_ID": "1"}):
            with self.assertRaises(RuntimeError): api.status(SHA, "success")
        self.assertEqual([x["state"] for x in writes], ["failure"])

    def test_unknown_current_review_is_pending_and_named_approval_can_clarify(self):
        unknown = review(CODEX, "COMMENTED")
        unknown["body"] = "Unclassified current finding"
        self.assertEqual(decision(pr(), [unknown], clean(), [])[0], "pending")
        self.assertEqual(decision(pr(), [unknown, review("leihaowen", id=2)], clean(), [])[0], "success")

    def test_bootstrap_failure_is_exported_for_runtime_isolation(self):
        import json
        case = WorkflowBootstrapTests()
        _, error = case.bootstrap([pr()], fail_sha=SHA)
        self.assertIsNone(error)
        self.assertEqual(json.loads(case.bootstrap_environment["NIMO_GATE_BOOTSTRAP_FAILURES"]), [SHA])


class BootstrapScopeRegressionTests(unittest.TestCase):
    def test_pr_specific_event_does_not_invalidate_other_pr_status(self):
        import json
        a, b = pr(), pr(); b.update(number=2, head={"sha":"b" * 40})
        case = WorkflowBootstrapTests()
        calls, error = case.bootstrap([a, b], event={"repository":{"default_branch":"main"}, "issue":{"number":1}})
        self.assertIsNone(error)
        self.assertEqual({c["head_sha"] for c in calls if "head_sha" in c}, {SHA})
        self.assertEqual(set(json.loads(case.bootstrap_environment["NIMO_GATE_BOOTSTRAP_HEADS"])), {SHA,"b"*40})

    def test_review_observer_title_selects_only_affected_pr(self):
        a, b = pr(), pr(); b.update(number=2, head={"sha":"b" * 40})
        calls, error = WorkflowBootstrapTests().bootstrap([a,b], event={"repository":{"default_branch":"main"},"workflow_run":{"display_title":"Review gate event for PR #2","pull_requests":[]}})
        self.assertIsNone(error)
        self.assertEqual({c["head_sha"] for c in calls if "head_sha" in c}, {"b"*40})

    def test_bootstrap_reuses_existing_pending_status(self):
        from review_gate import BRIDGE, BRIDGE_ID, STATUS_NAME
        current={"context":STATUS_NAME,"state":"pending","creator":{"login":BRIDGE,"id":BRIDGE_ID}}
        calls, error=WorkflowBootstrapTests().bootstrap([pr()],statuses=[current])
        self.assertIsNone(error)
        self.assertFalse(any(c.get("context")==STATUS_NAME for c in calls))

    def test_dynamic_head_is_invalidated_before_creating_diagnostic_check(self):
        import os,json
        from unittest.mock import patch
        api=GitHub.__new__(GitHub);api.repo="owner/repo"
        calls=[]
        api.status=lambda sha,state,**kwargs:calls.append(("status",sha,state))
        api.pages=lambda *args:[]
        api.call=lambda *args:{"id":1,"app":{"id":15368}}
        with patch.dict(os.environ,{"NIMO_GATE_BOOTSTRAP_HEADS":"[]","GITHUB_SERVER_URL":"https://github.com","GITHUB_RUN_ID":"1"}):api.check(SHA)
        self.assertEqual(calls,[("status",SHA,"pending")])

    def test_near_quota_bootstrap_blocks_before_status_capacity_is_exhausted(self):
        import json
        from review_gate import BRIDGE, BRIDGE_ID, STATUS_NAME, STATUS_HISTORY_LIMIT
        current={"context":STATUS_NAME,"state":"success","creator":{"login":BRIDGE,"id":BRIDGE_ID}}
        case=WorkflowBootstrapTests()
        calls,error=case.bootstrap([pr()],statuses=[current]*STATUS_HISTORY_LIMIT)
        self.assertIsNone(error)
        self.assertEqual([c["state"] for c in calls if c.get("context")==STATUS_NAME],["failure"])
        self.assertEqual(json.loads(case.bootstrap_environment["NIMO_GATE_BOOTSTRAP_FAILURES"]),[SHA])

    def test_failed_bootstrap_cohort_does_not_prevent_healthy_success(self):
        import os,json,tempfile
        from unittest.mock import patch
        from review_gate import run
        a,b=pr(),pr();b.update(number=2,head={"sha":"b"*40});a["updated_at"]=b["updated_at"]=TIME
        writes=[]
        class API:
            repo="owner/repo"
            def pages(self,path):return [a,b]
            def check(self,sha):return 2
            def call(self,path):return copy.deepcopy(b)
            def evidence(self,n):
                cs=copy.deepcopy(clean())
                for c in cs:c["verified_commit_id"]=b["head"]["sha"];c["body"]=c["body"].replace(SHA[:10],"b"*10).replace(SHA[:7],"b"*7)
                return {"reviews":[],"comments":cs,"threads":[]}
            def sync_approval(self,p,state,reason):writes.append(("approval",p["number"],state))
            def publish(self,c,state,reason):writes.append(("status",c,state))
        with tempfile.NamedTemporaryFile(mode="w") as f:
            json.dump({"repository":{"default_branch":"main"}},f);f.flush()
            with patch.dict(os.environ,{"GITHUB_EVENT_NAME":"push","GITHUB_EVENT_PATH":f.name,"NIMO_GATE_BOOTSTRAP_FAILURES":json.dumps([SHA])}),patch("review_gate.GitHub",return_value=API()):
                with self.assertRaises(RuntimeError):run()
        self.assertIn(("status",2,"success"),writes)
        self.assertFalse(any(kind=="approval" and number==1 for kind,number,state in writes))

    def test_second_recheck_also_validates_metadata_after_last_evidence(self):
        import os,json,tempfile
        from unittest.mock import patch
        from review_gate import run
        p=pr();p["updated_at"]=TIME;writes=[]
        class API:
            repo="owner/repo"
            reads=0
            evidence_reads=0
            def pages(self,path):return [p]
            def check(self,sha):return 1
            def call(self,path):
                self.reads+=1;q=copy.deepcopy(p)
                if self.reads>=4:q["updated_at"]="2026-10-08T10:00:01Z"
                if self.evidence_reads>=6:q["labels"]=[{"name":"hold"}]
                return q
            def evidence(self,n):
                self.evidence_reads+=1
                return {"reviews":[],"comments":clean(),"threads":[]}
            def sync_approval(self,p,state,reason):writes.append(("approval",state))
            def publish(self,c,state,reason):writes.append(("status",state))
        with tempfile.NamedTemporaryFile(mode="w") as f:
            json.dump({"repository":{"default_branch":"main"}},f);f.flush()
            with patch.dict(os.environ,{"GITHUB_EVENT_NAME":"push","GITHUB_EVENT_PATH":f.name}),patch("review_gate.GitHub",return_value=API()):run()
        self.assertIn(("status","failure"),writes)
        self.assertNotIn(("status","success"),writes)
        self.assertIn(("approval","failure"),writes)


class ReviewRoundAndFailureRegressionTests(unittest.TestCase):
    def test_new_request_cannot_use_late_verdict_of_previous_completed_round(self):
        cs=clean()
        cs[1]["created_at"]="2026-10-08T10:00:02Z"
        cs.append({"id":3,"user":user("developer"),"author_association":"MEMBER","body":"@codex review","created_at":"2026-10-08T10:00:01Z"})
        self.assertEqual(decision(pr(),[],cs,[])[0],"pending")

    def test_machine_comment_does_not_revoke_named_approval_of_unchanged_code(self):
        earlier=review("leihaowen")
        later=review(CODEX,"COMMENTED",id=2)
        later.update(submitted_at="2026-10-08T10:00:01Z",body="[P1] Later machine finding")
        self.assertEqual(decision(pr(),[earlier,later],clean(),[])[0],"success")
        changed=pr();changed["head"]["sha"]="f"*40
        self.assertNotEqual(decision(changed,[earlier,later],clean(),[])[0],"success")

    def test_history_read_failure_invalidates_status_and_managed_approval_only(self):
        case=WorkflowBootstrapTests()
        calls,error=case.bootstrap([pr()],history_failure=True,native_approval=True)
        self.assertIsNone(error)
        self.assertTrue(any(c.get("context")=="Nimo Review Approval" and c.get("state")=="failure" for c in calls))
        self.assertEqual(len([c for c in calls if "message" in c]),1)

    def test_gate_uses_supported_multi_pending_queue(self):
        from pathlib import Path
        base=Path(__file__).resolve().parent
        candidates=[base/"review-gate.yml",base.parent/"workflows/review-gate.yml"]
        workflow=next(p for p in candidates if p.exists()).read_text()
        self.assertIn("queue: max",workflow)
        self.assertIn("cancel-in-progress: false",workflow)


class FinalBodyAndQueueTests(unittest.TestCase):
    def test_claude_comment_review_p1_blocks_previous_formal_approval(self):
        earlier=review(CLAUDE)
        later=review(CLAUDE,"COMMENTED",id=2)
        later.update(body="[P1] Current commit blocker",submitted_at="2026-10-08T10:00:01Z")
        self.assertEqual(decision(pr(),[earlier,later],[],[])[0],"failure")

    def test_claude_p2_comment_review_preserves_valid_formal_approval(self):
        later=review(CLAUDE,"COMMENTED",id=2);later["body"]="[P2] Optional improvement"
        self.assertEqual(decision(pr(),[review(CLAUDE),later],[],[])[0],"success")

    def test_check_failure_preserves_native_approval_when_required_status_is_blocked(self):
        calls,error=WorkflowBootstrapTests().bootstrap([pr()],fail_sha=SHA,native_approval=True)
        self.assertIsNone(error)
        self.assertEqual(len([c for c in calls if "message" in c]),1)
        self.assertTrue(any(c.get("context")=="Nimo Review Approval" and c.get("state")=="pending" for c in calls))

    def test_invalidation_runs_outside_serial_publisher_queue(self):
        from pathlib import Path
        base=Path(__file__).resolve().parent
        candidates=[base/"review-gate.yml",base.parent/"workflows/review-gate.yml"]
        workflow=next(p for p in candidates if p.exists()).read_text()
        invalidation,publisher=workflow.split("  gate:\n",1)
        self.assertNotIn("concurrency:",invalidation)
        self.assertIn("Block previous verdicts before the publisher queue",invalidation)
        self.assertIn("needs: invalidate",publisher)
        self.assertIn("queue: max",publisher)
        self.assertIn("needs.invalidate.outputs.failures",publisher)


class ScopedPublisherRegressionTests(unittest.TestCase):
    def test_pr_specific_publisher_does_not_write_unrelated_pr_verdict(self):
        import os,json,tempfile
        from unittest.mock import patch
        from review_gate import run
        a,b=pr(),pr();b.update(number=2,head={"sha":"b"*40});a["updated_at"]=b["updated_at"]=TIME
        writes=[]
        class API:
            repo="owner/repo"
            def pages(self,path):return [a,b]
            def check(self,sha):return 2
            def call(self,path):return copy.deepcopy(b)
            def evidence(self,n):
                cs=copy.deepcopy(clean())
                for c in cs:c["verified_commit_id"]=b["head"]["sha"];c["body"]=c["body"].replace(SHA[:10],"b"*10).replace(SHA[:7],"b"*7)
                return {"reviews":[],"comments":cs,"threads":[]}
            def sync_approval(self,p,state,reason):writes.append(("approval",p["number"],state))
            def publish(self,c,state,reason):writes.append(("status",c,state))
        with tempfile.NamedTemporaryFile(mode="w") as f:
            json.dump({"repository":{"default_branch":"main"}},f);f.flush()
            with patch.dict(os.environ,{"GITHUB_EVENT_NAME":"push","GITHUB_EVENT_PATH":f.name,"NIMO_GATE_BOOTSTRAP_NUMBERS":json.dumps([2]),"NIMO_GATE_BOOTSTRAP_FAILURES":"[]"}),patch("review_gate.GitHub",return_value=API()):
                run()
        self.assertIn(("status",2,"success"),writes)
        self.assertFalse(any(number==1 for kind,number,state in writes))



class EpochAndSharedCommitRegressionTests(unittest.TestCase):
    def cohort_run(self, held=False, closed=False):
        import os,json,tempfile
        from unittest.mock import patch
        from review_gate import run
        a,b=pr(),pr();b["number"]=2
        a["updated_at"]=b["updated_at"]=TIME
        if held:b["labels"]=[{"name":"hold"}]
        pulls=[b] if closed else [a,b]
        writes=[]
        class API:
            repo="owner/repo"
            def pages(self,path):return pulls
            def check(self,sha):return 1
            def call(self,path):return copy.deepcopy(b if path.endswith("/2") else a)
            def evidence(self,n):return {"reviews":[],"comments":clean(),"threads":[]}
            def sync_approval(self,p,state,reason):writes.append(("approval",p["number"],state))
            def publish(self,c,state,reason):writes.append(("status",c,state))
        with tempfile.NamedTemporaryFile(mode="w") as f:
            json.dump({"repository":{"default_branch":"main"}},f);f.flush()
            env={"GITHUB_EVENT_NAME":"push","GITHUB_EVENT_PATH":f.name,
                 "NIMO_GATE_BOOTSTRAP_NUMBERS":"[1]","NIMO_GATE_BOOTSTRAP_AFFECTED_HEADS":json.dumps([SHA]),
                 "NIMO_GATE_BOOTSTRAP_FAILURES":"[]"}
            with patch.dict(os.environ,env),patch("review_gate.GitHub",return_value=API()):run()
        return writes

    def test_scoped_publisher_keeps_shared_sha_hold_in_cohort(self):
        writes=self.cohort_run(held=True)
        self.assertIn(("approval",2,"failure"),writes)
        self.assertIn(("status",1,"failure"),writes)
        self.assertNotIn(("status",1,"success"),writes)

    def test_closed_trigger_still_refreshes_remaining_shared_sha_pr(self):
        writes=self.cohort_run(closed=True)
        self.assertIn(("approval",2,"success"),writes)
        self.assertIn(("status",1,"success"),writes)

    def test_closed_pr_bootstrap_exports_remaining_shared_sha(self):
        import json
        b=pr();b["number"]=2
        case=WorkflowBootstrapTests()
        calls,error=case.bootstrap([b],event={"repository":{"default_branch":"main"},"pull_request":{"number":1,"head":{"sha":SHA}}})
        self.assertIsNone(error)
        self.assertEqual(json.loads(case.bootstrap_environment["NIMO_GATE_BOOTSTRAP_AFFECTED_HEADS"]),[SHA])
        self.assertTrue(any(c.get("context")=="Nimo Review Approval" and c.get("state")=="pending" for c in calls))

    def marker(self,epoch):
        from review_gate import BRIDGE,BRIDGE_ID,STATUS_NAME
        return {"context":STATUS_NAME,"state":"pending","description":"Review epoch "+epoch+": verifying evidence",
                "creator":{"login":BRIDGE,"id":BRIDGE_ID},"target_url":"https://github.com/owner/repo/actions/runs/"+epoch.split(".")[0]}

    def test_older_publisher_cannot_replace_new_generation(self):
        import os
        from unittest.mock import patch
        from review_gate import SupersededEvaluation
        api=GitHub.__new__(GitHub);api.repo="owner/repo";api.pages=lambda path:[self.marker("200.1")]
        api.call=lambda *args:self.fail("Superseded evaluation must not publish")
        with patch.dict(os.environ,{"NIMO_GATE_EPOCH":"100.1"}):
            with self.assertRaises(SupersededEvaluation):api.status(SHA,"success")

    def test_older_generation_cannot_create_formal_approval(self):
        import os
        from unittest.mock import patch
        from review_gate import SupersededEvaluation
        api=GitHub.__new__(GitHub);api.repo="owner/repo";api.pages=lambda path:[self.marker("200.1")]
        api.call=lambda *args:self.fail("Superseded evaluation must not approve")
        with patch.dict(os.environ,{"NIMO_GATE_EPOCH":"100.1"}):
            with self.assertRaises(SupersededEvaluation):api.sync_approval(pr(),"success","Codex completed review")

    def test_invalidation_between_preflight_and_post_is_restored(self):
        import os
        from unittest.mock import patch
        from review_gate import SupersededEvaluation,BRIDGE,BRIDGE_ID,STATUS_NAME
        api=GitHub.__new__(GitHub);api.repo="owner/repo";calls=[]
        older=self.marker("100.1");newer=self.marker("200.1")
        stale_green={**older,"state":"success","description":"Current-commit review policy passed"}
        histories=iter([[older],[stale_green,newer,older]])
        api.pages=lambda path:next(histories)
        def call(path,method,payload):
            calls.append(payload)
            return {**payload,"creator":{"login":BRIDGE,"id":BRIDGE_ID}}
        api.call=call
        with patch.dict(os.environ,{"NIMO_GATE_EPOCH":"100.1","GITHUB_SERVER_URL":"https://github.com","GITHUB_RUN_ID":"100"}):
            with self.assertRaises(SupersededEvaluation):api.status(SHA,"success")
        self.assertEqual([p["state"] for p in calls],["success","pending"])
        self.assertTrue(calls[-1]["description"].startswith("Review superseded epoch 100.1:"))
        self.assertEqual(api.epoch_marker([{**calls[-1], "creator": {"login": BRIDGE, "id": BRIDGE_ID}}, newer, older]), newer)

    def test_failed_post_write_verification_replaces_green_with_failure(self):
        import os
        from unittest.mock import patch
        from review_gate import BRIDGE,BRIDGE_ID
        api=GitHub.__new__(GitHub);api.repo="owner/repo";calls=[]
        api.pages=__import__("unittest.mock",fromlist=["Mock"]).Mock(side_effect=[[self.marker("100.1")],RuntimeError("read failed")])
        def call(path,method,payload):
            calls.append(payload);return {**payload,"creator":{"login":BRIDGE,"id":BRIDGE_ID}}
        api.call=call
        with patch.dict(os.environ,{"NIMO_GATE_EPOCH":"100.1","GITHUB_SERVER_URL":"https://github.com","GITHUB_RUN_ID":"100"}):
            with self.assertRaises(RuntimeError):api.status(SHA,"success")
        self.assertEqual([p["state"] for p in calls],["success","failure"])

    def test_new_bootstrap_generation_replaces_older_pending_marker(self):
        import os
        from unittest.mock import patch
        case=WorkflowBootstrapTests()
        with patch.dict(os.environ,{"NIMO_GATE_EPOCH":"200.1","GITHUB_RUN_ID":"200"}):
            calls,error=case.bootstrap([pr()],statuses=[self.marker("100.1")])
        self.assertIsNone(error)
        marks=[p for p in calls if p.get("context")=="Nimo Review Approval"]
        self.assertEqual(len(marks),1)
        self.assertTrue(marks[0]["description"].startswith("Review epoch 200.1:"))


class DynamicEpochInitializationTests(unittest.TestCase):
    def test_new_head_with_legacy_pending_still_receives_generation_marker(self):
        import os
        from unittest.mock import patch
        from review_gate import BRIDGE,BRIDGE_ID,STATUS_NAME
        api=GitHub.__new__(GitHub);api.repo="owner/repo";calls=[]
        legacy={"context":STATUS_NAME,"state":"pending","creator":{"login":BRIDGE,"id":BRIDGE_ID}}
        marker=EpochAndSharedCommitRegressionTests().marker("100.1")
        histories=iter([[legacy],[marker,legacy]])
        api.pages=lambda path:next(histories)
        def call(path,method,payload):
            calls.append(payload);return {**payload,"creator":{"login":BRIDGE,"id":BRIDGE_ID}}
        api.call=call
        with patch.dict(os.environ,{"NIMO_GATE_EPOCH":"100.1","GITHUB_SERVER_URL":"https://github.com","GITHUB_RUN_ID":"100"}):api.status(SHA,"pending",initialize=True)
        self.assertEqual(len(calls),1)
        self.assertTrue(calls[0]["description"].startswith("Review epoch 100.1:"))

    def initialization_api(self, old_epoch="99.1", created_at="2026-10-08T10:00:00Z", old_state="success"):
        from review_gate import BRIDGE, BRIDGE_ID, STATUS_NAME
        api = GitHub.__new__(GitHub); api.repo = "owner/repo"
        marker = {**EpochAndSharedCommitRegressionTests().marker(old_epoch), "id": 1, "created_at": created_at}
        history = [{"id": 2, "context": STATUS_NAME, "state": old_state,
                    "description": "Current-commit review policy passed",
                    "creator": {"login": BRIDGE, "id": BRIDGE_ID}}, marker]
        calls = []
        api.pages = lambda path, *args: list(history) if "/statuses" in path else []
        def call(path, method="GET", payload=None):
            calls.append((path, payload))
            result = {**(payload or {}), "id": 10 + len(calls), "created_at": "2026-10-08T11:00:01Z",
                      "creator": {"login": BRIDGE, "id": BRIDGE_ID}, "app": {"id": 15368}}
            if "/statuses/" in path: history.insert(0, result)
            return result
        api.call = call
        return api, calls, history

    def initialization_env(self):
        return {"NIMO_GATE_EPOCH": "100.1", "NIMO_GATE_BOOTSTRAP_STARTED_AT": "2026-10-08T11:00:00.500000+00:00",
                "NIMO_GATE_BOOTSTRAP_HEADS": "[]", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_RUN_ID": "100"}

    def test_reused_sha_old_green_is_invalidated_before_creating_check(self):
        import os
        from unittest.mock import patch
        api, calls, history = self.initialization_api()
        with patch.dict(os.environ, self.initialization_env()): api.check(SHA)
        self.assertEqual(calls[0][1]["state"], "pending")
        self.assertTrue(calls[0][1]["description"].startswith("Review epoch 100.1:"))
        self.assertIn("check-runs", calls[1][0])
        self.assertEqual(history[0]["state"], "pending")

    def test_older_pending_epoch_is_replaced_not_reused(self):
        import os
        from unittest.mock import patch
        api, calls, _ = self.initialization_api(old_state="pending")
        with patch.dict(os.environ, self.initialization_env()): api.status(SHA, "pending", initialize=True)
        self.assertEqual(len(calls), 1)
        self.assertTrue(calls[0][1]["description"].startswith("Review epoch 100.1:"))

    def test_lower_run_id_with_newer_invalidation_is_not_taken_over(self):
        import os
        from unittest.mock import patch
        from review_gate import SupersededEvaluation
        api, calls, _ = self.initialization_api(old_epoch="50.2", created_at="2026-10-08T11:00:01Z")
        with patch.dict(os.environ, self.initialization_env()):
            with self.assertRaises(SupersededEvaluation): api.check(SHA)
        self.assertEqual(calls, [])

    def test_same_second_marker_blocks_green_without_claiming_epoch(self):
        import os
        from unittest.mock import patch
        from review_gate import SupersededEvaluation
        api, calls, history = self.initialization_api(created_at="2026-10-08T11:00:00Z")
        with patch.dict(os.environ, self.initialization_env()):
            with self.assertRaises(SupersededEvaluation): api.check(SHA)
        self.assertEqual([p["state"] for _, p in calls], ["pending"])
        self.assertNotIn("Review epoch", calls[0][1]["description"])
        self.assertEqual(api.epoch_marker(history)["description"].split(":")[0], "Review epoch 99.1")

    def test_missing_marker_time_cannot_leave_old_green(self):
        import os
        from unittest.mock import patch
        from review_gate import SupersededEvaluation
        api, calls, _ = self.initialization_api(created_at=None)
        with patch.dict(os.environ, self.initialization_env()):
            with self.assertRaises(SupersededEvaluation): api.check(SHA)
        self.assertEqual([p["state"] for _, p in calls], ["pending"])

    def test_invalidation_racing_initialization_is_not_hidden_by_own_marker(self):
        import os
        from unittest.mock import patch
        from review_gate import SupersededEvaluation
        api, calls, history = self.initialization_api()
        call = api.call
        newer = {**EpochAndSharedCommitRegressionTests().marker("101.1"), "id": 20, "created_at": "2026-10-08T11:00:01Z"}
        def racing_call(path, method="GET", payload=None):
            if not calls: history.insert(0, newer)
            return call(path, method, payload)
        api.call = racing_call
        with patch.dict(os.environ, self.initialization_env()):
            with self.assertRaises(SupersededEvaluation): api.check(SHA)
        self.assertEqual([p["state"] for _, p in calls], ["pending", "pending"])
        self.assertTrue(calls[-1][1]["description"].startswith("Review superseded epoch 100.1:"))
        self.assertEqual(api.epoch_marker(history)["id"], newer["id"])

    def test_partial_rerun_uses_successful_invalidation_epoch_and_start(self):
        import os, pathlib, re
        from unittest.mock import patch
        case = WorkflowBootstrapTests()
        with patch.dict(os.environ, {"NIMO_GATE_EPOCH": "100.1", "GITHUB_RUN_ID": "100"}):
            _, error = case.bootstrap([pr()])
        self.assertIsNone(error)
        self.assertEqual(case.bootstrap_outputs["epoch"], "100.1")
        self.assertEqual(case.bootstrap_outputs["started_at"], case.bootstrap_environment["NIMO_GATE_BOOTSTRAP_STARTED_AT"])
        base = pathlib.Path(__file__).resolve().parent
        workflow = next(p for p in [base/"review-gate.yml", base.parent/"workflows/review-gate.yml"] if p.exists()).read_text()
        publisher = workflow.split("  gate:\n", 1)[1]
        mapped = {key: case.bootstrap_outputs[out] for key, out in re.findall(
            r"^      (NIMO_GATE_[A-Z_]+): \$\{\{ needs.invalidate.outputs.([a-z_]+) \}\}", publisher, re.M)}
        api, calls, _ = self.initialization_api(old_epoch="100.1")
        # On rerun github.run_attempt is 2, but invalidate was not rerun.
        with patch.dict(os.environ, {"NIMO_GATE_EPOCH": "100.2", **mapped}): api.assert_current_epoch(SHA)
        self.assertEqual(calls, [])

    def third_generation_race(self, initialize):
        import os
        from unittest.mock import patch
        from review_gate import SupersededEvaluation
        api, calls, history = self.initialization_api(old_epoch="100.1", created_at="2026-10-08T10:00:00Z")
        if initialize:
            history[-1]["description"] = "Review epoch 99.1: previous use of SHA"
        else:
            history[0]["state"] = "pending"
        call = api.call
        second = {**EpochAndSharedCommitRegressionTests().marker("50.2"), "id": 20, "created_at": "2026-10-08T11:00:01Z"}
        third = {**EpochAndSharedCommitRegressionTests().marker("25.3"), "id": 30, "created_at": "2026-10-08T11:00:02Z"}
        def racing_call(path, method="GET", payload=None):
            # B invalidates after A's preflight. C invalidates after A reads B
            # but just before A writes its compensating pending status.
            if len(calls) == 0: history.insert(0, second)
            if len(calls) == 1: history.insert(0, third)
            return call(path, method, payload)
        api.call = racing_call
        with patch.dict(os.environ, self.initialization_env()):
            with self.assertRaises(SupersededEvaluation):
                api.status(SHA, "pending" if initialize else "success", initialize=initialize)
        self.assertEqual(history[0]["state"], "pending")
        self.assertTrue(history[0]["description"].startswith("Review superseded epoch 100.1:"))
        self.assertEqual(api.epoch_marker(history)["id"], third["id"])
        with patch.dict(os.environ, {**self.initialization_env(), "NIMO_GATE_EPOCH": "50.2"}):
            with self.assertRaises(SupersededEvaluation): api.status(SHA, "success")
        # C remains authoritative even though its run ID is the smallest.
        with patch.dict(os.environ, {**self.initialization_env(), "NIMO_GATE_EPOCH": "25.3"}):
            api.assert_current_epoch(SHA)
        self.assertEqual(len(calls), 2)

    def test_third_invalidation_survives_cached_normal_compensation(self):
        self.third_generation_race(initialize=False)

    def test_third_invalidation_survives_hidden_initialization_compensation(self):
        self.third_generation_race(initialize=True)
