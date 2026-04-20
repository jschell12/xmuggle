const fs = require('fs');
const path = require('path');
const os = require('os');
const { execSync } = require('child_process');
const crypto = require('crypto');

const API_KEY_FILE = path.join(os.homedir(), '.xmuggle', 'api-key');
const WORK_DIR = path.join(os.homedir(), '.xmuggle', 'work');
const API_URL = 'https://api.anthropic.com/v1/messages';
const MODEL = 'claude-sonnet-4-20250514';
const MAX_TOKENS = 8192;

const SYSTEM_PROMPT = `You are a code fix agent. You analyze screenshots showing bugs, UI issues, or errors, and fix the code.

When you identify what needs to change, use the edit_file tool to make each edit. You can call edit_file multiple times for multiple files.

Rules:
- Only change what is necessary to fix the issue shown in the screenshot.
- Provide the complete new file content (not a diff).
- Write a clear summary of what you changed and why.
- If the screenshot is unclear, explain what you see and do not make changes.`;

const EDIT_FILE_TOOL = {
  name: 'edit_file',
  description: 'Replace the contents of a file to fix the issue shown in the screenshot',
  input_schema: {
    type: 'object',
    properties: {
      path: { type: 'string', description: 'File path relative to repo root' },
      content: { type: 'string', description: 'The complete new file content' },
      summary: { type: 'string', description: 'Brief description of what changed in this file' },
    },
    required: ['path', 'content', 'summary'],
  },
};

function getApiKey() {
  if (process.env.ANTHROPIC_API_KEY) return process.env.ANTHROPIC_API_KEY;
  try {
    return fs.readFileSync(API_KEY_FILE, 'utf8').trim();
  } catch {
    return null;
  }
}

function setApiKey(key) {
  const dir = path.dirname(API_KEY_FILE);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(API_KEY_FILE, key.trim() + '\n', { mode: 0o600 });
}

function resetApiKey() {
  try { fs.unlinkSync(API_KEY_FILE); } catch {}
}

function hasApiKey() {
  return !!getApiKey();
}

function mediaType(filePath) {
  const ext = path.extname(filePath).toLowerCase();
  const types = { '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.webp': 'image/webp', '.gif': 'image/gif' };
  return types[ext] || 'image/png';
}

function repoURL(slug) {
  if (slug.startsWith('http') || slug.startsWith('git@')) return slug;
  return `git@github.com:${slug}.git`;
}

function getRepoContext(repoRoot) {
  let files;
  try {
    files = execSync('git ls-files', { cwd: repoRoot, encoding: 'utf8', maxBuffer: 1024 * 1024 }).trim().split('\n');
  } catch {
    files = [];
  }

  const skipDirs = ['node_modules', 'vendor', '.git', 'bin', 'dist', 'build'];
  const sourceExts = ['.js', '.ts', '.tsx', '.jsx', '.go', '.py', '.css', '.html', '.json', '.md', '.yaml', '.yml', '.toml'];
  const maxFileSize = 50_000;
  const maxTotalSize = 200_000;

  let totalSize = 0;
  const fileContents = [];

  for (const f of files) {
    if (skipDirs.some(d => f.startsWith(d + '/'))) continue;
    if (!sourceExts.some(ext => f.endsWith(ext))) continue;

    const full = path.join(repoRoot, f);
    try {
      const stat = fs.statSync(full);
      if (stat.size > maxFileSize) continue;
      if (totalSize + stat.size > maxTotalSize) break;
      const content = fs.readFileSync(full, 'utf8');
      fileContents.push({ path: f, content });
      totalSize += stat.size;
    } catch {}
  }

  return { files, fileContents };
}

async function analyzeAndFix({ imagePaths, projectPath, message, onProgress }) {
  const log = onProgress || (() => {});
  const apiKey = getApiKey();
  if (!apiKey) throw new Error('No API key. Set ANTHROPIC_API_KEY or save to ~/.xmuggle/api-key');
  if (!projectPath) throw new Error('No project specified');

  // Derive repo slug from project's git remote
  log('Resolving repo from git remote…');
  let repo;
  try {
    const remote = execSync('git remote get-url origin', { cwd: projectPath, encoding: 'utf8' }).trim();
    repo = remote.replace(/^https:\/\/github\.com\//, '').replace(/^git@github\.com:/, '').replace(/\.git$/, '');
  } catch {
    repo = path.basename(projectPath);
  }
  log(`Repo: ${repo}`);

  // Clone repo to temp dir
  fs.mkdirSync(WORK_DIR, { recursive: true });
  const id = crypto.randomBytes(4).toString('hex');
  const cloneDir = path.join(WORK_DIR, `${repo.replace(/\//g, '-')}-${id}`);
  const branch = `xmuggle-fix-${id}`;

  log(`Cloning ${repo} (shallow)…`);
  try {
    execSync(`git clone --depth 1 ${repoURL(repo)} "${cloneDir}"`, { encoding: 'utf8', stdio: 'pipe' });
  } catch (e) {
    throw new Error(`Clone failed: ${e.stderr || e.message}`);
  }
  log('Clone complete');

  // Create branch
  log(`Creating branch ${branch}…`);
  execSync(`git checkout -b ${branch}`, { cwd: cloneDir, stdio: 'pipe' });

  // Build image blocks
  log(`Encoding ${imagePaths.length} image(s)…`);
  const imageBlocks = imagePaths.map(p => ({
    type: 'image',
    source: {
      type: 'base64',
      media_type: mediaType(p),
      data: fs.readFileSync(p).toString('base64'),
    },
  }));

  // Gather repo context
  log('Gathering repo context (file list + source files)…');
  const ctx = getRepoContext(cloneDir);
  log(`Found ${ctx.files.length} tracked files, reading ${ctx.fileContents.length} source files`);
  let contextText = `Repository: ${repo}\n\nFiles in repo:\n${ctx.files.join('\n')}\n\n`;

  for (const f of ctx.fileContents) {
    contextText += `--- ${f.path} ---\n${f.content}\n\n`;
  }

  if (message) {
    contextText += `\nUser context: ${message}\n`;
  }

  contextText += '\nAnalyze the screenshot(s) and fix the issue using the edit_file tool.';

  // Call API
  log(`Calling Claude API (${MODEL})…`);
  const body = {
    model: MODEL,
    max_tokens: MAX_TOKENS,
    system: SYSTEM_PROMPT,
    tools: [EDIT_FILE_TOOL],
    tool_choice: { type: 'auto' },
    messages: [{
      role: 'user',
      content: [...imageBlocks, { type: 'text', text: contextText }],
    }],
  };

  const resp = await fetch(API_URL, {
    method: 'POST',
    headers: {
      'x-api-key': apiKey,
      'anthropic-version': '2023-06-01',
      'content-type': 'application/json',
    },
    body: JSON.stringify(body),
  });

  if (!resp.ok) {
    fs.rmSync(cloneDir, { recursive: true, force: true });
    const err = await resp.text();
    throw new Error(`API error ${resp.status}: ${err}`);
  }

  log('API response received, parsing…');
  const result = await resp.json();

  // Extract edits
  const edits = [];
  let summary = '';

  for (const block of result.content) {
    if (block.type === 'tool_use' && block.name === 'edit_file') {
      edits.push(block.input);
    }
    if (block.type === 'text') {
      summary += block.text;
    }
  }

  if (edits.length === 0) {
    log('No edits returned — cleaning up');
    fs.rmSync(cloneDir, { recursive: true, force: true });
    return { status: 'no_changes', summary: summary || 'No changes needed.' };
  }

  // Apply edits
  log(`Applying ${edits.length} edit(s)…`);
  const changedFiles = [];
  for (const edit of edits) {
    const fullPath = path.join(cloneDir, edit.path);
    fs.mkdirSync(path.dirname(fullPath), { recursive: true });
    fs.writeFileSync(fullPath, edit.content);
    changedFiles.push(edit.path);
    log(`  ✎ ${edit.path}: ${edit.summary}`);
  }

  // Commit, push, create PR
  const commitSummary = edits.map(e => e.summary).join('; ');
  const commitMsg = `fix: ${commitSummary}`;
  let prUrl = '';

  try {
    log('Staging and committing…');
    for (const f of changedFiles) {
      execSync(`git add -- "${f}"`, { cwd: cloneDir, stdio: 'pipe' });
    }
    execSync(`git commit -m "${commitMsg.replace(/"/g, '\\"')}"`, { cwd: cloneDir, stdio: 'pipe' });

    log(`Pushing branch ${branch}…`);
    execSync(`git push -u origin ${branch}`, { cwd: cloneDir, stdio: 'pipe' });

    // Create PR via gh CLI
    log('Creating pull request…');
    const prBody = `## Screenshot fix\n\n${summary}\n\n## Changes\n${changedFiles.map(f => '- ' + f).join('\n')}\n\n---\nAutomated fix by xmuggle`;
    const prOutput = execSync(
      `gh pr create --title "${commitMsg.replace(/"/g, '\\"')}" --body "${prBody.replace(/"/g, '\\"')}"`,
      { cwd: cloneDir, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] }
    ).trim();
    // gh pr create prints the URL as the last line
    const lines = prOutput.split('\n');
    prUrl = lines[lines.length - 1];
    log(`PR created: ${prUrl}`);
  } catch (e) {
    log(`Push/PR failed: ${e.stderr || e.message}`);
    // Clean up but still report what happened
    fs.rmSync(cloneDir, { recursive: true, force: true });
    return {
      status: 'push_failed',
      summary: `Edits applied but push/PR failed: ${e.stderr || e.message}`,
      changedFiles,
    };
  }

  // Clean up clone
  log('Cleaning up temp clone…');
  fs.rmSync(cloneDir, { recursive: true, force: true });
  log('Done!');

  return {
    status: 'success',
    summary: commitSummary,
    prUrl,
    changedFiles,
    analysisText: summary,
  };
}

module.exports = { getApiKey, setApiKey, resetApiKey, hasApiKey, analyzeAndFix };
