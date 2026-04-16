import { resolve, dirname, join } from "node:path";
import { existsSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { buildPrompt } from "./prompt.js";
import { spawnAgent } from "./spawn.js";
import { createTaskId, writeTask, type TaskPayload } from "./queue.js";
import {
  sendTask,
  pollForResult,
  discoverHost,
  type RemoteTarget,
} from "./remote.js";
import {
  findLatestImage,
  findAllUnprocessed,
  findImageByName,
  autoIngestScreenshots,
  ingestFromScanDirs,
  markProcessed,
  listAllImages,
} from "./images.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const MAC_LINK = resolve(__dirname, "..", "scripts", "mac-link.sh");

const USAGE = `Usage: look --repo <repo> [--img <name>]... [--all] [--msg "context"] [--remote [--host <host>] [--user <user>]] [--list] [--scan]

  --repo  <repo>   GitHub repo (owner/name or URL) or local path
  --img   <name>   Select image(s) by name or partial match (repeatable)
  --all            Process ALL unprocessed images (not just the latest)
  --msg   <msg>    Optional context to guide the agent
  --remote         Send to a remote machine for processing
  --host  <host>   Remote hostname/IP (with --remote). If omitted, runs
                   mac-link.sh to discover Macs on the LAN
  --user  <user>   Remote SSH user (defaults to current \$USER)
  --list           List all images in ~/.look/ and their status
  --scan           Scan ~/Desktop and ~/Downloads for ALL images (not just screenshots)

Image detection:
  New screenshots are auto-detected from ~/Desktop and ~/Downloads via
  macOS Spotlight (kMDItemIsScreenCapture) and copied into ~/.look/.
  No manual step needed — just take a screenshot and run the command.

  --scan is only needed to ingest non-screenshot images (downloads, etc).

Examples:
  look --repo jschell12/my-app                        # latest new screenshot
  look --repo jschell12/my-app --all                  # all new screenshots
  look --repo jschell12/my-app --msg "fix the btn"    # with context
  look --repo jschell12/my-app --img bug1 --img bug2  # multiple images
  look --repo jschell12/my-app --remote               # pick remote host on LAN
  look --repo jschell12/my-app --remote --host mac.local
  look --list                                         # see images + status`;

function parseArgs(argv: string[]) {
  const args = argv.slice(2);

  if (args.includes("--help") || args.includes("-h")) {
    console.log(USAGE);
    process.exit(0);
  }

  let repo: string | undefined;
  let message: string | undefined;
  let host: string | undefined;
  let user: string | undefined;
  let remote = false;
  let list = false;
  let scan = false;
  let all = false;
  const imgs: string[] = [];

  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg === "--repo" && i + 1 < args.length) {
      repo = args[++i];
    } else if (arg === "--msg" && i + 1 < args.length) {
      message = args[++i];
    } else if (arg === "--img" && i + 1 < args.length) {
      imgs.push(args[++i]);
    } else if (arg === "--host" && i + 1 < args.length) {
      host = args[++i];
    } else if (arg === "--user" && i + 1 < args.length) {
      user = args[++i];
    } else if (arg === "--remote") {
      remote = true;
    } else if (arg === "--list") {
      list = true;
    } else if (arg === "--scan") {
      scan = true;
    } else if (arg === "--all") {
      all = true;
    }
  }

  return { repo, message, host, user, remote, list, scan, all, imgs };
}

/** Resolve --img arguments to absolute paths in the image store */
function resolveImages(imgs: string[]): string[] {
  const resolved: string[] = [];
  for (const query of imgs) {
    const found = findImageByName(query);
    if (!found) {
      console.error(`Error: no image matching "${query}" in ~/.look/`);
      console.error("Run --list to see available images.");
      process.exit(1);
    }
    resolved.push(found.path);
  }
  return resolved;
}

async function runLocal(
  screenshotPaths: string[],
  repo: string,
  message?: string
): Promise<void> {
  const prompt = buildPrompt({ screenshotPaths, repo, message });
  const exitCode = await spawnAgent({ prompt });
  for (const p of screenshotPaths) markProcessed(p);
  process.exit(exitCode);
}

async function resolveRemoteTarget(
  host?: string,
  user?: string
): Promise<RemoteTarget> {
  let resolvedHost = host;
  if (!resolvedHost) {
    console.log("Discovering Macs on the LAN...");
    const discovered = await discoverHost(MAC_LINK);
    if (!discovered) {
      console.error("Error: no host selected. Use --host to specify one.");
      process.exit(1);
    }
    resolvedHost = discovered;
  }
  return { host: resolvedHost, user };
}

async function runRemote(
  screenshotPaths: string[],
  repo: string,
  message: string | undefined,
  host: string | undefined,
  user: string | undefined
): Promise<void> {
  const target = await resolveRemoteTarget(host, user);
  console.log(`Remote: ${user ? user + "@" : ""}${target.host}`);

  const taskIds: string[] = [];
  for (const screenshotPath of screenshotPaths) {
    const taskId = createTaskId();
    const tmpBase = join(tmpdir(), "look-tasks");
    mkdirSync(tmpBase, { recursive: true });

    const payload: TaskPayload = {
      repo,
      message,
      timestamp: Date.now(),
      status: "pending",
    };

    const taskDir = writeTask(tmpBase, taskId, payload, screenshotPath);
    console.log(`Sending task ${taskId}...`);

    await sendTask(target, taskDir, taskId);
    taskIds.push(taskId);
    markProcessed(screenshotPath);
  }

  console.log(`${taskIds.length} task(s) sent. Waiting for results...`);

  let failed = false;
  for (const taskId of taskIds) {
    const result = await pollForResult(target, taskId);
    console.log(`\n--- Task ${taskId} ---`);

    if (result.status === "success") {
      console.log("Fix applied successfully!");
      if (result.pr_url) console.log(`PR: ${result.pr_url}`);
      if (result.branch) console.log(`Branch: ${result.branch}`);
    } else {
      console.error("Agent reported an error:");
      console.error(result.summary.slice(-500));
      failed = true;
    }
  }

  if (existsSync(resolve(repo))) {
    console.log(`\nPulling latest in ${repo}...`);
    const pull = spawn("git", ["pull"], {
      cwd: resolve(repo),
      stdio: "inherit",
    });
    await new Promise<void>((res) => pull.on("close", () => res()));
  }

  if (failed) process.exit(1);
}

async function main() {
  const { repo, message, host, user, remote, list, scan, all, imgs } =
    parseArgs(process.argv);

  if (scan) {
    const count = ingestFromScanDirs();
    console.log(`Ingested ${count} image(s) into ~/.look/`);
    if (!repo) process.exit(0);
  }

  if (list) {
    const ingested = autoIngestScreenshots();
    if (ingested > 0) console.log(`Auto-ingested ${ingested} new screenshot(s)\n`);

    const images = listAllImages();
    if (images.length === 0) {
      console.log("No images in ~/.look/");
      console.log("Take a screenshot, or run --scan to ingest all images from Desktop/Downloads.");
    } else {
      const unprocessed = images.filter((i) => !i.isProcessed).length;
      console.log(
        `${images.length} image(s) in ~/.look/ (${unprocessed} unprocessed):\n`
      );
      for (const img of images) {
        const status = img.isProcessed ? "done" : "pending";
        console.log(`  [${status}] ${img.name}`);
      }
    }
    process.exit(0);
  }

  if (!repo) {
    console.error("Error: --repo is required\n");
    console.log(USAGE);
    process.exit(1);
  }

  let screenshotPaths: string[];

  if (imgs.length > 0) {
    screenshotPaths = resolveImages(imgs);
  } else if (all) {
    const unprocessed = findAllUnprocessed();
    if (unprocessed.length === 0) {
      console.error("No unprocessed images in ~/.look/");
      console.error("Take a screenshot, or run --scan to ingest from Desktop/Downloads.");
      process.exit(1);
    }
    screenshotPaths = unprocessed.map((img) => img.path);
    console.log(`Found ${screenshotPaths.length} unprocessed image(s)`);
  } else {
    const found = findLatestImage();
    if (!found) {
      console.error("No unprocessed images in ~/.look/");
      console.error("Take a screenshot, or run --scan to ingest from Desktop/Downloads.");
      process.exit(1);
    }
    screenshotPaths = [found.path];
  }

  const names = screenshotPaths.map((p) => p.split("/").pop());
  console.log(`Screenshot(s): ${names.join(", ")}`);
  console.log(`Target repo: ${repo}`);
  if (message) console.log(`Context: ${message}`);
  console.log(`Mode: ${remote ? "remote" : "local"}`);
  console.log("---");

  if (remote) {
    await runRemote(screenshotPaths, repo, message, host, user);
  } else {
    await runLocal(screenshotPaths, repo, message);
  }
}

main().catch((err) => {
  console.error(err.message || err);
  process.exit(1);
});
