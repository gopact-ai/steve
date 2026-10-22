---
name: steve-review
description: Review a Steve pull request against the repository's requirements and report evidence-backed findings.
---

1. Read the pull request description, its full diff and the base branch code
   the diff relies on. Check that the stated tests exist and cover the change.
2. Look for reproducible defects: behavior that contradicts the description,
   broken contracts between Go and the console, missing idempotency or recovery,
   credentials reaching logs or output, and stale embedded console builds.
3. For each finding give the trigger, the impact and the file and line. Mark
   untested hypotheses as such. Skip style preferences the code base does not
   follow.
4. Return findings through Steve's normal result. Preparing findings does not
   authorize approving, merging or pushing.
