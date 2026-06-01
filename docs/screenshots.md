# Screenshots

A visual tour of the rubbish UI in the browser.

---

## Sessions — Waiting for SSH

A newly created session in the **PLAN** stage, waiting for the VM to come up and accept an SSH connection.

![Session waiting for SSH](assets/screenshot-session-waiting.png)

---

## Sessions — Multiple Implement Workers Ready

Four parallel **IMPLEMENT** sessions all in the **READY** state. Each session is an isolated VM running a fan-out worker from the same workflow.

![Multiple implement sessions ready](assets/screenshot-sessions-ready.png)

---

## Workflow Detail — Completed Fan-out

A completed workflow showing the full execution graph: a single **research** stage, a single **plan** stage, and eight parallel **implement** workers that ran concurrently. Status is **DONE**.

![Completed workflow with parallel implement stages](assets/screenshot-workflow-detail.png)
