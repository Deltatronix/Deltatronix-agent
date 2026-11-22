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

## Deploying backend and realtime

Agents deploy the backend and realtime services themselves, without asking Dian
and without a 1Password popup (Dian, 2026-11-22). The `deploy` user's SSH key is
read from `~/.ssh/realtime_deploy_key` on this Mac (`IdentityAgent none` in
`~/.ssh/config`), not from 1Password.

1. Merge, put the repo's main checkout on the merged `main` (clean tree, equal
   to `origin/main`), and run `./deploy.sh` there. It runs the whole gate (lint,
   tests, build), then ships. Fix gate failures; never bypass the gate.
2. Run one deploy per service at a time. If another agent is deploying the same
   service, wait for it to finish.
3. Report the deployed SHA, then verify on the platform (see "Complete the
   delivery cycle").

`--dry-run` still runs the gate without touching a server. `--auth` and `--ship`
still work but are no longer needed. If a deploy shows a 1Password popup or
fails with "communication with agent failed", the SSH config was changed back:
tell Dian, and don't work around it.

`publish.sh` scripts still read their token from 1Password (`op read`) as their
first step, so they prompt at once; run them only after Dian says he is there.

## Required models

- **Coding and development:** GPT-6.1 Sol (`gpt-6.1-sol`).
- **Manual testing on the deployed platform:** GPT-6.1 Terra (`gpt-6.1-terra`).

Use these models when assigning or launching the corresponding work. An agent
running another model must hand that work to the required model. Preserve these
assignments in worker prompts and handoffs. If a required model is unavailable,
report the blocker instead of silently substituting another model or assigning
manual testing to Dian. Preserve all clean-room and licensing restrictions when
dispatching development work.
