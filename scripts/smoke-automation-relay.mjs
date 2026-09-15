// Run only against a disposable local Kandev instance. No Bitbucket access.
import assert from "node:assert/strict";
import { createHmac, randomUUID } from "node:crypto";
import { readFile, mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execFileSync } from "node:child_process";

const base = process.env.KANDEV_PLUGIN_E2E_URL;
const packagePath = process.env.KANDEV_PLUGIN_E2E_PACKAGE;
assert(
  base && packagePath,
  "Set KANDEV_PLUGIN_E2E_URL and KANDEV_PLUGIN_E2E_PACKAGE for a disposable host",
);
assert(
  ["localhost", "127.0.0.1", "[::1]"].includes(new URL(base).hostname),
  "This smoke test requires a disposable loopback host",
);
const pluginID = "kandev-plugin-bitbucket";
const pluginPath = `/api/plugins/${pluginID}`;
const webhookPath = `${pluginPath}/webhooks/automation-relay`;
async function http(path, options = {}, expected = 200) {
  const response = await fetch(new URL(path, base), {
    ...options,
    signal: AbortSignal.timeout(20000),
  });
  assert.equal(
    response.status,
    expected,
    `${options.method ?? "GET"} ${path}: unexpected HTTP status`,
  );
  const text = await response.text();
  return text ? JSON.parse(text) : null;
}
function json(method, body) {
  return {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  };
}
const existing = await http("/api/plugins");
assert(
  !existing.plugins.some((p) => p.id === pluginID),
  "Refusing to change an existing Bitbucket installation; use a fresh host",
);
const socket = new WebSocket(new URL("/ws", base).href.replace(/^http/, "ws"));
await new Promise((resolve, reject) => {
  socket.addEventListener("open", resolve, { once: true });
  socket.addEventListener("error", reject, { once: true });
});
const pending = new Map();
socket.addEventListener("message", ({ data }) => {
  const message = JSON.parse(data);
  const callback = pending.get(message.id);
  if (callback) {
    pending.delete(message.id);
    callback(message);
  }
});
function ws(action, payload) {
  return new Promise((resolve, reject) => {
    const id = randomUUID();
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`${action} timed out`));
    }, 20000);
    pending.set(id, (message) => {
      clearTimeout(timer);
      if (message.type === "error")
        reject(new Error(`${action}: ${message.payload?.message}`));
      else resolve(message.payload);
    });
    socket.send(JSON.stringify({ id, type: "request", action, payload }));
  });
}
let installed = false,
  workspace,
  automation,
  repositoryPath;
try {
  const upload = new FormData();
  upload.set(
    "package",
    new Blob([await readFile(packagePath)]),
    "bitbucket.tar.gz",
  );
  await http("/api/plugins/install", { method: "POST", body: upload }, 201);
  installed = true;
  assert.equal((await http(pluginPath)).status, "active");
  workspace = await http(
    "/api/v1/workspaces",
    json("POST", { name: "Relay smoke test" }),
  );
  repositoryPath = await mkdtemp(join(tmpdir(), "kandev-relay-repo-"));
  execFileSync("git", ["init", "-b", "main", repositoryPath], {
    stdio: "ignore",
  });
  await writeFile(
    join(repositoryPath, "README.md"),
    "Disposable relay fixture\n",
  );
  execFileSync("git", ["-C", repositoryPath, "add", "README.md"]);
  execFileSync(
    "git",
    [
      "-C",
      repositoryPath,
      "-c",
      "user.name=Relay test",
      "-c",
      "user.email=relay@example.invalid",
      "commit",
      "-m",
      "Fixture",
    ],
    { stdio: "ignore" },
  );
  const repository = await http(
    `/api/v1/workspaces/${workspace.id}/repositories`,
    json("POST", {
      name: "relay-fixture",
      source_type: "local",
      local_path: repositoryPath,
      default_branch: "main",
      pull_before_worktree: false,
    }),
    201,
  );
  const agents = await http("/api/v1/agents");
  const profile = agents.agents.find((a) => a.name === "mock-agent")
    ?.profiles?.[0];
  assert(
    profile,
    "Run the disposable host with KANDEV_MOCK_AGENT=only and its mock-agent binary available",
  );
  automation = await ws("automation.create", {
    workspace_id: workspace.id,
    name: "Signed Bitbucket relay smoke",
    prompt: "Repository: {{webhook.repository.full_name}}",
    agent_profile_id: profile.id,
    repository_ids: [repository.id],
    max_concurrent_runs: 10,
    triggers: [{ type: "webhook", config: {}, enabled: true }],
  });
  assert(automation.webhook_secret);
  const secret = randomUUID();
  const config = {
    relay_enabled: true,
    relay_product: "cloud",
    relay_repository: "team/repo",
    relay_event: "push",
    relay_branches: "main",
    relay_destination_url: new URL(
      `/api/v1/automations/webhook/${automation.id}`,
      base,
    ).href,
    relay_signing_secret: secret,
    relay_automation_secret: automation.webhook_secret,
  };
  await http(pluginPath, json("PATCH", { config }));
  const masked = JSON.stringify(await http(`${pluginPath}/config`));
  assert(
    !masked.includes(secret) && !masked.includes(automation.webhook_secret),
    "Secrets must be masked",
  );
  const body = JSON.stringify({
    repository: { full_name: "team/repo" },
    push: {
      changes: [
        { new: { type: "branch", name: "main", target: { hash: "abc" } } },
      ],
    },
  });
  const signature =
    "sha256=" + createHmac("sha256", secret).update(body).digest("hex");
  const delivery = {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Event-Key": "repo:push",
      "X-Hub-Signature": signature,
    },
    body,
  };
  await http(
    webhookPath,
    { ...delivery, headers: { "X-Event-Key": "repo:push" } },
    401,
  );
  await http(webhookPath, { ...delivery, body: body + " " }, 401);
  assert.equal((await http(webhookPath, delivery)).status, "forwarded");
  const runs = () =>
    ws("automation.runs.list", { automation_id: automation.id });
  let firstRuns = [];
  for (let i = 0; i < 40; i++) {
    firstRuns = await runs();
    if (firstRuns?.[0]?.task_id) break;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  assert.equal(firstRuns.length, 1, "A real automation run must be admitted");
  assert(firstRuns[0].task_id, "The automation must create a task");
  const task = await http(`/api/v1/tasks/${firstRuns[0].task_id}`);
  assert.equal(
    task.description,
    "Repository: team/repo",
    "Raw webhook fields must reach the automation template",
  );
  assert.equal((await http(webhookPath, delivery)).status, "duplicate");
  await http(`${pluginPath}/disable`, { method: "POST" });
  await http(`${pluginPath}/enable`, { method: "POST" });
  assert.equal((await http(webhookPath, delivery)).status, "duplicate");
  config.relay_branches = "other";
  await http(pluginPath, json("PATCH", { config }));
  await http(webhookPath, delivery, 204);
  config.relay_enabled = false;
  await http(pluginPath, json("PATCH", { config }));
  await http(webhookPath, delivery, 404);
  assert.equal(
    (await runs()).length,
    1,
    "Duplicates, unmatched and disabled deliveries must not add runs",
  );
} finally {
  try {
    if (installed) await http(pluginPath, { method: "DELETE" });
    if (automation) await ws("automation.delete", { id: automation.id });
    if (workspace)
      await http(
        `/api/v1/workspaces/${workspace.id}`,
        json("DELETE", { confirm_name: workspace.name }),
      );
  } finally {
    socket.close();
    if (repositoryPath)
      await rm(repositoryPath, { recursive: true, force: true });
  }
}

console.log(
  "PASS: packaged plugin activation, secret masking, signed forwarding, task creation and template interpolation, duplicate suppression across restart, filtering, disable, and cleanup.",
);
