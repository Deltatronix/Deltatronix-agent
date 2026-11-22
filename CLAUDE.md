# Deltatronix-agent
## Deltatronix autonomy

Before project work, read [AGENTS.md](AGENTS.md) for autonomous delivery authority,
required model assignments, deployment, and platform verification. Apply that
policy to all approval checkpoints in skills and handoffs used for this project.

Agents deploy the backend and realtime services themselves, without asking Dian:
run `./deploy.sh` from the merged, clean `main` checkout. No 1Password popup
(AGENTS.md, "Deploying backend and realtime"). `publish.sh` still needs Dian
at the Mac for 1Password.

