import { spawn } from "node:child_process";
import type { TaskResult } from "./queue.js";

export interface RemoteTarget {
  host: string;
  user?: string;
  queueDir?: string;
  resultsDir?: string;
}

function sshTarget(t: RemoteTarget): string {
  return t.user ? `${t.user}@${t.host}` : t.host;
}

function queueDir(t: RemoteTarget): string {
  return t.queueDir ?? "~/.look/queue";
}

function resultsDir(t: RemoteTarget): string {
  return t.resultsDir ?? "~/.look/results";
}

function exec(
  cmd: string,
  args: string[],
  timeoutMs = 30_000
): Promise<{ stdout: string; stderr: string; code: number }> {
  return new Promise((resolve, reject) => {
    const child = spawn(cmd, args, {
      stdio: ["ignore", "pipe", "pipe"],
      timeout: timeoutMs,
    });

    const stdout: Buffer[] = [];
    const stderr: Buffer[] = [];

    child.stdout!.on("data", (c) => stdout.push(c));
    child.stderr!.on("data", (c) => stderr.push(c));

    child.on("error", reject);
    child.on("close", (code) => {
      resolve({
        stdout: Buffer.concat(stdout).toString("utf-8"),
        stderr: Buffer.concat(stderr).toString("utf-8"),
        code: code ?? 1,
      });
    });
  });
}

/** rsync a local task directory to the remote queue */
export async function sendTask(
  target: RemoteTarget,
  taskDir: string,
  taskId: string
): Promise<void> {
  const sshT = sshTarget(target);
  const remotePath = `${queueDir(target)}/${taskId}/`;

  await exec("ssh", [sshT, `mkdir -p ${queueDir(target)}/${taskId}`]);

  const { code, stderr } = await exec("rsync", [
    "-az",
    `${taskDir}/`,
    `${sshT}:${remotePath}`,
  ]);

  if (code !== 0) {
    throw new Error(`rsync failed (exit ${code}): ${stderr}`);
  }
}

/** Poll the remote machine for a result file */
export async function pollForResult(
  target: RemoteTarget,
  taskId: string,
  opts?: { timeoutMs?: number; pollIntervalMs?: number }
): Promise<TaskResult> {
  const timeoutMs = opts?.timeoutMs ?? 600_000;
  const pollMs = opts?.pollIntervalMs ?? 5_000;
  const sshT = sshTarget(target);
  const resultPath = `${resultsDir(target)}/${taskId}/result.json`;
  const start = Date.now();

  while (Date.now() - start < timeoutMs) {
    const { stdout, code } = await exec("ssh", [
      sshT,
      `cat ${resultPath} 2>/dev/null`,
    ]);

    if (code === 0 && stdout.trim()) {
      try {
        return JSON.parse(stdout.trim());
      } catch {
        // keep polling
      }
    }

    process.stderr.write(".");
    await new Promise((r) => setTimeout(r, pollMs));
  }

  throw new Error(
    `Timed out waiting for result after ${timeoutMs / 1000}s. Is the daemon running on ${target.host}?`
  );
}

/** Test SSH connectivity */
export async function testConnection(target: RemoteTarget): Promise<boolean> {
  try {
    const { stdout, code } = await exec(
      "ssh",
      [sshTarget(target), "echo ok"],
      10_000
    );
    return code === 0 && stdout.trim() === "ok";
  } catch {
    return false;
  }
}

/**
 * Invoke scripts/mac-link.sh discover to let the user pick a host interactively.
 * Returns the hostname they chose, or null if cancelled.
 */
export function discoverHost(macLinkPath: string): Promise<string | null> {
  return new Promise((resolve) => {
    const child = spawn(macLinkPath, ["discover"], {
      stdio: ["inherit", "pipe", "inherit"],
    });

    const out: Buffer[] = [];
    child.stdout!.on("data", (c) => out.push(c));
    child.on("close", (code) => {
      if (code !== 0) return resolve(null);
      const host = Buffer.concat(out).toString("utf-8").trim();
      resolve(host || null);
    });
    child.on("error", () => resolve(null));
  });
}
