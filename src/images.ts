import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  statSync,
  writeFileSync,
  appendFileSync,
} from "node:fs";
import { execSync } from "node:child_process";
import { homedir } from "node:os";
import { basename, extname, join } from "node:path";

const IMAGE_EXTS = new Set([".png", ".jpg", ".jpeg", ".webp", ".gif"]);

/** Central image store */
const IMAGE_DIR = join(homedir(), ".look");
const TRACKED_FILE = join(IMAGE_DIR, ".tracked");
const SEEN_FILE = join(IMAGE_DIR, ".seen");

function isImage(filename: string): boolean {
  return IMAGE_EXTS.has(extname(filename).toLowerCase());
}

function ensureImageDir(): void {
  mkdirSync(IMAGE_DIR, { recursive: true });
  if (!existsSync(TRACKED_FILE)) {
    writeFileSync(TRACKED_FILE, "# Processed images — one filename per line\n");
  }
  if (!existsSync(SEEN_FILE)) {
    writeFileSync(SEEN_FILE, "# Seen source paths — prevents re-ingesting\n");
  }
}

/** Filenames already marked as processed */
function loadTracked(): Set<string> {
  ensureImageDir();
  const set = new Set<string>();
  for (const line of readFileSync(TRACKED_FILE, "utf-8").split("\n")) {
    const t = line.trim();
    if (t && !t.startsWith("#")) set.add(t);
  }
  return set;
}

/** Source paths already ingested (so we don't copy the same file twice) */
function loadSeen(): Set<string> {
  ensureImageDir();
  const set = new Set<string>();
  for (const line of readFileSync(SEEN_FILE, "utf-8").split("\n")) {
    const t = line.trim();
    if (t && !t.startsWith("#")) set.add(t);
  }
  return set;
}

function markSeen(srcPath: string): void {
  ensureImageDir();
  appendFileSync(SEEN_FILE, srcPath + "\n");
}

export function markProcessed(absPath: string): void {
  ensureImageDir();
  appendFileSync(TRACKED_FILE, basename(absPath) + "\n");
}

/** List images in a directory, sorted by mtime descending */
function listImages(dir: string): { path: string; name: string; mtime: number }[] {
  if (!existsSync(dir)) return [];
  const entries = readdirSync(dir, { withFileTypes: true });
  const images: { path: string; name: string; mtime: number }[] = [];

  for (const entry of entries) {
    if (!entry.isFile()) continue;
    if (entry.name.startsWith(".")) continue;
    if (!isImage(entry.name)) continue;
    const fullPath = join(dir, entry.name);
    const stat = statSync(fullPath);
    images.push({ path: fullPath, name: entry.name, mtime: stat.mtimeMs });
  }

  return images.sort((a, b) => b.mtime - a.mtime);
}

/**
 * Copy an image into ~/.look/ (copy, not move — leave originals).
 * Returns the new path. Deduplicates by name.
 */
function ingestImage(srcPath: string): string {
  ensureImageDir();
  let destName = basename(srcPath);
  let destPath = join(IMAGE_DIR, destName);

  if (existsSync(destPath)) {
    const ext = extname(destName);
    const stem = destName.slice(0, -ext.length);
    destName = `${stem}-${Date.now()}${ext}`;
    destPath = join(IMAGE_DIR, destName);
  }

  copyFileSync(srcPath, destPath);
  markSeen(srcPath);
  return destPath;
}

/**
 * Use macOS Spotlight to find screenshots that haven't been ingested yet.
 * Uses kMDItemIsScreenCapture metadata — only finds actual screenshots,
 * not arbitrary images.
 *
 * Falls back to filename pattern matching on non-macOS or if mdfind fails.
 */
function findNewScreenshots(): string[] {
  const seen = loadSeen();
  let candidates: string[] = [];

  try {
    const output = execSync(
      `mdfind 'kMDItemIsScreenCapture = 1' -onlyin "${homedir()}/Desktop"`,
      { encoding: "utf-8", timeout: 10_000 }
    );
    candidates = output.trim().split("\n").filter(Boolean);
  } catch {
    // Fallback: scan Desktop for Screenshot*.png pattern
    for (const img of listImages(join(homedir(), "Desktop"))) {
      if (img.name.startsWith("Screenshot")) {
        candidates.push(img.path);
      }
    }
  }

  return candidates.filter((p) => existsSync(p) && !seen.has(p));
}

/**
 * Find new images in ~/Desktop that haven't been ingested.
 * Unlike findNewScreenshots(), this finds ALL images, not just screenshots.
 */
function findNewImages(): string[] {
  const seen = loadSeen();
  const results: string[] = [];

  for (const img of listImages(join(homedir(), "Desktop"))) {
    if (!seen.has(img.path)) results.push(img.path);
  }

  return results;
}

/**
 * Auto-ingest new screenshots from ~/Desktop into the store.
 * Called automatically before image resolution — no explicit --scan needed.
 * Only ingests actual screenshots (via Spotlight metadata).
 * Returns count of newly ingested images.
 */
export function autoIngestScreenshots(): number {
  const newShots = findNewScreenshots();
  let count = 0;
  for (const src of newShots) {
    ingestImage(src);
    count++;
  }
  return count;
}

/**
 * Scan ~/Desktop for ALL new images (not just screenshots).
 * Used by --scan for explicit bulk ingest.
 * Returns count of newly ingested images.
 */
export function ingestFromScanDirs(): number {
  const newImages = findNewImages();
  let count = 0;
  for (const src of newImages) {
    ingestImage(src);
    count++;
  }
  return count;
}

export interface DiscoveredImage {
  path: string;
  name: string;
  isProcessed: boolean;
}

/**
 * Find the latest unprocessed image in the store.
 * Auto-ingests new screenshots first.
 */
export function findLatestImage(): DiscoveredImage | null {
  autoIngestScreenshots();
  ensureImageDir();
  const tracked = loadTracked();
  const images = listImages(IMAGE_DIR);

  for (const img of images) {
    if (!tracked.has(img.name)) {
      return { path: img.path, name: img.name, isProcessed: false };
    }
  }

  return null;
}

/**
 * Find ALL unprocessed images in the store.
 * Auto-ingests new screenshots first.
 */
export function findAllUnprocessed(): DiscoveredImage[] {
  autoIngestScreenshots();
  return listUnprocessed();
}

/** List all images with status. Does NOT auto-ingest. */
export function listAllImages(): DiscoveredImage[] {
  ensureImageDir();
  const tracked = loadTracked();
  const images = listImages(IMAGE_DIR);

  return images.map((img) => ({
    path: img.path,
    name: img.name,
    isProcessed: tracked.has(img.name),
  }));
}

/** List unprocessed images only. Does NOT auto-ingest. */
export function listUnprocessed(): DiscoveredImage[] {
  return listAllImages().filter((img) => !img.isProcessed);
}

/**
 * Resolve an image by name or partial match within the store.
 * Auto-ingests new screenshots first.
 */
export function findImageByName(query: string): DiscoveredImage | null {
  autoIngestScreenshots();
  ensureImageDir();
  const tracked = loadTracked();
  const images = listImages(IMAGE_DIR);

  // Exact match
  for (const img of images) {
    if (img.name === query) {
      return { path: img.path, name: img.name, isProcessed: tracked.has(img.name) };
    }
  }

  // Prefix match
  const prefixMatches = images.filter((img) =>
    img.name.toLowerCase().startsWith(query.toLowerCase())
  );
  if (prefixMatches.length === 1) {
    const img = prefixMatches[0];
    return { path: img.path, name: img.name, isProcessed: tracked.has(img.name) };
  }

  // Substring match — return newest
  const subMatches = images.filter((img) =>
    img.name.toLowerCase().includes(query.toLowerCase())
  );
  if (subMatches.length > 0) {
    const img = subMatches[0];
    return { path: img.path, name: img.name, isProcessed: tracked.has(img.name) };
  }

  return null;
}
