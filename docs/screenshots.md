# Screenshots

A visual tour of the rubbish UI in the browser.

---

## New Session — Launch Flow (animated)

Click **+ New Session**, fill in a repo, and hit **Launch Session**. The VM boots, progresses through provisioning → configuring → ready, and the browser navigates directly to a live Claude Code terminal — all in under 5 seconds.

![Animated demo of session launch flow](assets/demo-new-session.gif)

---

## New Session Modal

The **New Session** dialog. Repo URL is pre-filled from the last used value; branch is optional; **Dev mode** injects Claude memories and the deploy key into the VM at boot.

![New session modal with repo URL and dev mode checkbox](assets/screenshot-new-session-modal.png)

---

## Sessions — Waiting for SSH

A newly created session in the **PLAN** stage, waiting for the VM to come up and accept an SSH connection.

![Session waiting for SSH](assets/screenshot-session-waiting.png)

---

## Sessions — Multiple Implement Workers Ready

Four parallel **IMPLEMENT** sessions all in the **READY** state. Each session is an isolated VM running a fan-out worker from the same workflow.

![Multiple implement sessions ready](assets/screenshot-sessions-ready.png)

---

## Interactive Terminal — Claude Running

A live Claude Code session inside a Firecracker microVM. The terminal proxies WebSocket → SSH into the VM; Claude starts automatically with `--dangerously-skip-permissions`.

![Interactive Claude Code terminal inside a VM](assets/screenshot-session-launching.png)

---

## Workflow Detail — Completed Fan-out

A completed workflow showing the full execution graph: a single **research** stage, a single **plan** stage, and eight parallel **implement** workers that ran concurrently. Status is **DONE**.

![Completed workflow with parallel implement stages](assets/screenshot-workflow-detail.png)
