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
  return `https://github.com/${slug}.git`;
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

async function analyzeAndFix({ imagePaths, repo, message }) {
  const apiKey = getApiKey();
  if (!apiKey) throw new Error('No API key. Set ANTHROPIC_API_KEY or save to ~/.xmuggle/api-key');
  if (!repo) throw new Error('No repo specified');

  // Clone repo to temp dir
  fs.mkdirSync(WORK_DIR, { recursive: true });
  const id = crypto.randomBytes(4).toString('hex');
  const cloneDir = path.join(WORK_DIR, `${repo.replace(/\//g, '-')}-${id}`);
  const branch = `xmuggle-fix-${id}`;

  try {
    execSync(`git clone --depth 1 ${repoURL(repo)} "${cloneDir}"`, { encoding: 'utf8', stdio: 'pipe' });
  } catch (e) {
    throw new Error(`Clone failed: ${e.stderr || e.message}`);
  }

  // Create branch
  execSync(`git checkout -b ${branch}`, { cwd: cloneDir, stdio: 'pipe' });

  // Build image blocks
  const imageBlocks = imagePaths.map(p => ({
    type: 'image',
    source: {
      type: 'base64',
      media_type: mediaType(p),
      data: fs.readFileSync(p).toString('base64'),
    },
  }));

  // Gather repo context
  const ctx = getRepoContext(cloneDir);
  let contextText = `Repository: ${repo}\n\nFiles in repo:\n${ctx.files.join('\n')}\n\n`;

  for (const f of ctx.fileContents) {
    contextText += `--- ${f.path} ---\n${f.content}\n\n`;
  }

  if (message) {
    contextText += `\nUser context: ${message}\n`;
  }

  contextText += '\nAnalyze the screenshot(s) and fix the issue using the edit_file tool.';

  // Call API
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
    fs.rmSync(cloneDir, { recursive: true, force: true });
    return { status: 'no_changes', summary: summary || 'No changes needed.' };
  }

  // Apply edits
  const changedFiles = [];
  for (const edit of edits) {
    const fullPath = path.join(cloneDir, edit.path);
    fs.mkdirSync(path.dirname(fullPath), { recursive: true });
    fs.writeFileSync(fullPath, edit.content);
    changedFiles.push(edit.path);
  }

  // Commit, push, create PR
  const commitSummary = edits.map(e => e.summary).join('; ');
  const commitMsg = `fix: ${commitSummary}`;
  let prUrl = '';

  try {
    for (const f of changedFiles) {
      execSync(`git add -- "${f}"`, { cwd: cloneDir, stdio: 'pipe' });
    }
    execSync(`git commit -m "${commitMsg.replace(/"/g, '\\"')}"`, { cwd: cloneDir, stdio: 'pipe' });
    execSync(`git push -u origin ${branch}`, { cwd: cloneDir, stdio: 'pipe' });

    // Create PR via gh CLI
    const prBody = `## Screenshot fix\n\n${summary}\n\n## Changes\n${changedFiles.map(f => '- ' + f).join('\n')}\n\n---\nAutomated fix by xmuggle`;
    const prOutput = execSync(
      `gh pr create --title "${commitMsg.replace(/"/g, '\\"')}" --body "${prBody.replace(/"/g, '\\"')}"`,
      { cwd: cloneDir, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'] }
    ).trim();
    // gh pr create prints the URL as the last line
    const lines = prOutput.split('\n');
    prUrl = lines[lines.length - 1];
  } catch (e) {
    // Clean up but still report what happened
    fs.rmSync(cloneDir, { recursive: true, force: true });
    return {
      status: 'push_failed',
      summary: `Edits applied but push/PR failed: ${e.stderr || e.message}`,
      changedFiles,
    };
  }

  // Clean up clone
  fs.rmSync(cloneDir, { recursive: true, force: true });

  return {
    status: 'success',
    summary: commitSummary,
    prUrl,
    changedFiles,
    analysisText: summary,
  };
}

module.exports = { getApiKey, setApiKey, resetApiKey, hasApiKey, analyzeAndFix };
