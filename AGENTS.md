# Deltatronix autonomous delivery

## Scope and authority

These instructions apply only to the Deltatronix repositories, their worktrees,
GitHub development work, and the online Deltatronix test platform. The platform
is online for testing and is **not in production**.

Dian authorizes agents to complete project work proactively, from investigation
through deployment and verification. Create and manage issues, post development
comments, commit and push changes, create and update PRs, address reviews and CI,
and merge after required checks pass. Proceed without draft approval or further
permission. This project authorization replaces approval checkpoints in inherited
posting rules, skills, agent instructions, handoffs, and scheduled runs for these
actions. Other projects and unrelated communications retain their existing rules.

Create, edit, and delete projects and any other entities on the non-production
platform as needed to implement and test the task, including destructive test
cases. These operations are authorized. Keep test data identifiable and clean up
temporary entities when verification finishes.

## Complete the delivery cycle

1. Investigate the task, choose the implementation, and make the changes. Resolve
   routine technical and product choices from project context without asking Dian.
2. Run the repository's required checks, review the changes, fix failures, push,
   create the PR, and merge when required checks and reviews pass. Keep branch
   protections and licensing rules intact. Preserve other agents' work and use
   an isolated worktree for development.
3. Deploy using the repository's documented release process, including dependent
   packages and services. An automatic deployment after merge counts as deployment
   only after its success and deployed revision have been verified.
4. Test the changed behavior on the deployed platform and check affected workflows.
   Exercise relevant creation, editing, persistence/reload, and deletion flows.
   Local tests and a successful deployment alone do not complete a behavior change.
5. Fix failures, redeploy, and repeat platform testing until it passes. Report the
   merged work, deployed revision or release, tested workflows, results, and any
   remaining blocker. Documentation-only changes require instruction/diff checks;
   they do not require deploying unchanged application code.

## Authentication and blockers

Deployment is already authorized. Prepare and execute it autonomously. When
1Password requires physical authentication, tell Dian the concrete authentication
step needed, pause only the blocked operation, and resume deployment and testing
after authentication. Never request or expose secret values. Continue independent
work while access is blocked. Report genuine access or tooling failures precisely;
do not turn them into approval requests or claim unperformed verification passed.

## Deploying while Dian is away

Backend and realtime deploys (`Deltatronix-backend/deploy.sh`,
`Deltatronix-realtime/deploy.sh`) need Dian to approve the SSH key in a
1Password popup. He must see that popup within a minute of saying he is at the
Mac, so do every slow step before you ask him:

1. **Stage first.** Merge, put the repo's main checkout on the merged `main`, and
   run `./deploy.sh --dry-run` there. It runs the whole gate (lint, tests, build)
   and leaves `release.tar.gz` built for HEAD without contacting a server. Fix
   failures before asking.
2. **Ask once:** "Backend `<sha>` is staged; tell me when you're at the Mac for
   the 1Password popup." While waiting, leave the staged checkout alone (no pull,
   rebuild or branch switch) and keep working elsewhere.
3. **When Dian says he is there, your first command is `./deploy.sh --auth`** in
   each repo you will ship, back to back: no reply, test, build, pull or agent
   spawn first. It only opens the SSH connection, so the popup shows within
   seconds. An orchestrator runs it itself; waking or spawning a worker first
   costs minutes. The popup expires after about 60 seconds; if `--auth` fails
   with "communication with agent failed", tell Dian and run it again at once.
4. **Then run `./deploy.sh --ship`.** It skips the gate and ships the staged
   tarball over the approved connection. That connection stays open for 30
   minutes, so retries and a second repo need no new popup.
5. If `--ship` refuses because `main` moved, run `--dry-run` again, then
   `--ship`. No new popup is needed inside the 30 minutes.

`publish.sh` scripts read their token from 1Password as their first step, so
they prompt at once; run them only after Dian says he is there.

## Required models

- **Coding and development:** GPT-6.1 Sol (`gpt-6.1-sol`).
- **Manual testing on the deployed platform:** GPT-6.1 Terra (`gpt-6.1-terra`).

Use these models when assigning or launching the corresponding work. An agent
running another model must hand that work to the required model. Preserve these
assignments in worker prompts and handoffs. If a required model is unavailable,
report the blocker instead of silently substituting another model or assigning
manual testing to Dian. Preserve all clean-room and licensing restrictions when
dispatching development work.
