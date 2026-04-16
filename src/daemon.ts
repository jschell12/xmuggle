import { spawn } from "node:child_process";
import { existsSync, readdirSync } from "node:fs";
import { homedir } from "node:os";
import { join, basename } from "node:path";
import { QUEUE_DIR, RESULTS_DIR, ensureDirs } from "./config.js";
import {
  findScreenshot,
  listPendingTasks,
  readTask,
  updateTaskStatus,
} from "./queue.js";
import { buildWorkerPrompt } from "./prompt.js";
import { spawnAgentCapture } from "./spawn.js";

const AQ_SCRIPTS =
  process.env.AQ_SCRIPTS ??
  join(homedir(), "development/github.com/jschell12/agent-queue/scripts");

const MAX_WORKERS = parseInt(process.env.MAX_WORKERS ?? "3", 10);

function log(msg: string): void {
  const ts = new Date().toISOString();
  console.log(`[${ts}] ${msg}`);
}

/** Run an agent-queue command and return stdout */
function aq(args: string[]): Promise<{ stdout: string; code: number }> {
  return new Promise((resolve, reject) => {
    const child = spawn(join(AQ_SCRIPTS, "agent-queue"), args, {
      stdio: ["ignore", "pipe", "pipe"],
      env: { ...process.env },
    });
    const out: Buffer[] = [];
    child.stdout!.on("data", (c) => out.push(c));
    child.on("error", reject);
    child.on("close", (code) =>
      resolve({ stdout: Buffer.concat(out).toString("utf-8"), code: code ?? 1 })
    );
  });
}

/** Derive project name from repo (e.g., "jschell12/my-app" → "my-app") */
function projectName(repo: string): string {
  const cleaned = repo
    .replace(/^https:\/\/github\.com\//, "")
    .replace(/^git@github\.com:/, "")
    .replace(/\.git$/, "");
  return basename(cleaned);
}

/** Derive GitHub clone URL */
function repoUrl(repo: string): string {
  if (repo.startsWith("http") || repo.startsWith("git@")) return repo;
  if (/^[\w-]+\/[\w.-]+$/.test(repo))
    return `https://github.com/${repo}.git`;
  return repo;
}

/** Get count of active workers for a project from agent-queue */
async function activeWorkerCount(project: string): Promise<number> {
  const { stdout, code } = await aq([
    "list",
    "-p",
    project,
    "--status",
    "in-progress",
    "--json",
  ]);
  if (code !== 0) return 0;
  try {
    const items = JSON.parse(stdout);
    const agents = new Set(
      items.map((i: { agent?: string }) => i.agent).filter(Boolean)
    );
    return agents.size;
  } catch {
    return 0;
  }
}

/** Get the next available agent ID */
async function nextAgentId(project: string): Promise<string> {
  const { stdout } = await aq(["list", "-p", project, "--json"]);
  let maxId = 0;
  try {
    const items = JSON.parse(stdout);
    for (const item of items) {
      if (item.agent) {
        const m = item.agent.match(/agent-(\d+)/);
        if (m) maxId = Math.max(maxId, parseInt(m[1], 10));
      }
    }
  } catch {
    // ignore
  }
  return `agent-${maxId + 1}`;
}

/** Add a screenshot task to the agent-queue */
async function enqueueTask(
  taskDir: string,
  project: string
): Promise<boolean> {
  const taskId = basename(taskDir);
  const task = readTask(taskDir);
  const screenshot = findScreenshot(taskDir);

  if (!screenshot) {
    log(`No screenshot in ${taskDir}, skipping`);
    updateTaskStatus(taskDir, "error");
    return false;
  }

  // Init queue if needed (idempotent)
  await aq(["init", "-p", project]);

  // Add item with title encoding the task-id
  const title = `look-fix:${taskId}`;
  const desc = task.message ?? "Screenshot-driven fix";
  const { code } = await aq([
    "add",
    title,
    desc,
    "-p",
    project,
    "--tags",
    "screenshot",
  ]);

  if (code === 0) {
    log(`Enqueued task ${taskId} to agent-queue project ${project}`);
    updateTaskStatus(taskDir, "processing");
    return true;
  } else {
    log(`Failed to enqueue task ${taskId}`);
    return false;
  }
}

/** Spawn a worker agent for the project */
async function spawnWorker(
  project: string,
  repo: string
): Promise<void> {
  const agentId = await nextAgentId(project);
  const url = repoUrl(repo);

  // Clone repo into isolated directory
  const { stdout, code } = await aq([
    "clone",
    url,
    agentId,
    "--parent",
    "/tmp",
  ]);

  if (code !== 0) {
    log(`Failed to clone for ${agentId}: ${stdout}`);
    return;
  }

  let cloneDir: string;
  let branch: string;
  try {
    const info = JSON.parse(stdout);
    cloneDir = info.clone_dir;
    branch = info.branch;
  } catch {
    log(`Failed to parse clone output for ${agentId}: ${stdout}`);
    return;
  }

  log(`Spawning worker ${agentId} in ${cloneDir} on branch ${branch}`);

  const prompt = buildWorkerPrompt({
    agentId,
    project,
    repoUrl: url,
    cloneDir,
    branch,
    aqScripts: AQ_SCRIPTS,
    screenshotQueueDir: QUEUE_DIR,
    resultsDir: RESULTS_DIR,
  });

  // Fire and forget — worker runs independently
  spawnAgentCapture({ prompt, cwd: cloneDir }).then(
    (result) => {
      log(
        `Worker ${agentId} exited with code ${result.exitCode}`
      );
    },
    (err) => {
      log(`Worker ${agentId} error: ${err.message}`);
    }
  );
}

/** Group pending screenshot tasks by project (repo) */
function groupByProject(
  taskDirs: string[]
): Map<string, { taskDir: string; repo: string }[]> {
  const groups = new Map<string, { taskDir: string; repo: string }[]>();
  for (const taskDir of taskDirs) {
    try {
      const task = readTask(taskDir);
      const project = projectName(task.repo);
      if (!groups.has(project)) groups.set(project, []);
      groups.get(project)!.push({ taskDir, repo: task.repo });
    } catch {
      // skip malformed
    }
  }
  return groups;
}

async function tick(): Promise<void> {
  const pending = listPendingTasks(QUEUE_DIR);
  if (pending.length === 0) return;

  const groups = groupByProject(pending);

  for (const [project, tasks] of groups) {
    // Enqueue all pending tasks
    let enqueued = 0;
    for (const { taskDir } of tasks) {
      if (await enqueueTask(taskDir, project)) enqueued++;
    }

    if (enqueued === 0) continue;

    // Check active workers, spawn more if needed
    const active = await activeWorkerCount(project);
    const needed = Math.min(enqueued, MAX_WORKERS) - active;

    for (let i = 0; i < needed; i++) {
      await spawnWorker(project, tasks[0].repo);
    }

    log(
      `Project ${project}: ${enqueued} tasks queued, ${active} workers active, ${Math.max(0, needed)} spawned`
    );
  }
}

async function pollLoop(intervalMs: number): Promise<void> {
  log(
    `Daemon started (agent-queue mode). Watching ${QUEUE_DIR} every ${intervalMs / 1000}s`
  );
  log(`Agent-queue scripts: ${AQ_SCRIPTS}`);
  log(`Max workers: ${MAX_WORKERS}`);

  if (!existsSync(join(AQ_SCRIPTS, "agent-queue"))) {
    log(
      `WARNING: agent-queue script not found at ${AQ_SCRIPTS}/agent-queue. Set AQ_SCRIPTS env var.`
    );
  }

  const timer = setInterval(async () => {
    try {
      await tick();
    } catch (err) {
      log(`Tick error: ${err instanceof Error ? err.message : err}`);
    }
  }, intervalMs);

  // Run immediately
  try {
    await tick();
  } catch (err) {
    log(`Initial tick error: ${err instanceof Error ? err.message : err}`);
  }

  const shutdown = () => {
    log("Shutting down...");
    clearInterval(timer);
    process.exit(0);
  };

  process.on("SIGTERM", shutdown);
  process.on("SIGINT", shutdown);
}

const intervalMs = parseInt(process.env.POLL_INTERVAL_MS ?? "5000", 10);
ensureDirs();
pollLoop(intervalMs);
