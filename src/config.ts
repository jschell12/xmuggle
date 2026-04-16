import { mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

export const CONFIG_DIR = join(homedir(), ".look");
export const QUEUE_DIR = join(CONFIG_DIR, "queue");
export const RESULTS_DIR = join(CONFIG_DIR, "results");
export const LOGS_DIR = join(CONFIG_DIR, "logs");

export function ensureDirs(): void {
  for (const dir of [CONFIG_DIR, QUEUE_DIR, RESULTS_DIR, LOGS_DIR]) {
    mkdirSync(dir, { recursive: true });
  }
}
