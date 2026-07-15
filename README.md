# devplat-agent

Go agent for devplat's Firecracker microVM orchestration. Runs directly on each
data-plane host (Host A, Host B, ...), talks to Firecracker over its REST API,
and is controlled by the scheduler (in `devplat-backend`) over the WireGuard
tunnel. See the `claude/firecracker-orchestration` branch / pull request for
the initial implementation.
