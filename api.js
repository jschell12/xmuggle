const fs = require('fs');
const path = require('path');
const os = require('os');
const { execSync } = require('child_process');
const crypto = require('crypto');

const API_KEY_FILE = path.join(os.homedir(), '.xmuggle', 'api-key');
const GH_TOKEN_FILE = path.join(os.homedir(), '.xmuggle', 'gh-token');
const MODEL_FILE = path.join(os.homedir(), '.xmuggle', 'model');
const HISTORY_DIR = path.join(os.homedir(), '.xmuggle', 'history');
const WORK_DIR = path.join(os.homedir(), '.xmuggle', 'work');
const API_URL = 'https://api.anthropic.com/v1/messages';
const DEFAULT_MODEL = 'claude-sonnet-4-6';
const MAX_TOKENS = 8192;

const AVAILABLE_MODELS = [
  { id: 'claude-opus-4-6', label: 'Opus 4.6' },
  { id: 'claude-sonnet-4-6', label: 'Sonnet 4.6' },
  { id: 'claude-sonnet-4-20250514', label: 'Sonnet 4' },
  { id: 'claude-haiku-4-5-20251001', label: 'Haiku 4.5' },
];

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

function getGhToken() {
  if (process.env.GH_TOKEN) return process.env.GH_TOKEN;
  if (process.env.GITHUB_TOKEN) return process.env.GITHUB_TOKEN;
  try {
    return fs.readFileSync(GH_TOKEN_FILE, 'utf8').trim();
  } catch {
    return null;
  }
}

function setGhToken(token) {
  const dir = path.dirname(GH_TOKEN_FILE);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(GH_TOKEN_FILE, token.trim() + '\n', { mode: 0o600 });
}

function resetGhToken() {
  try { fs.unlinkSync(GH_TOKEN_FILE); } catch {}
}

function hasGhToken() {
  return !!getGhToken();
}

function getModel() {
  try {
    const saved = fs.readFileSync(MODEL_FILE, 'utf8').trim();
    if (AVAILABLE_MODELS.some(m => m.id === saved)) return saved;
  } catch {}
  return DEFAULT_MODEL;
}

function setModel(modelId) {
  const dir = path.dirname(MODEL_FILE);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(MODEL_FILE, modelId.trim() + '\n');
}

function listModels() {
  return AVAILABLE_MODELS;
}

function gitEnv() {
  const token = getGhToken();
  if (!token) return process.env;
  return {
    ...process.env,
    GH_TOKEN: token,
    GIT_ASKPASS: 'echo',
    GIT_TERMINAL_PROMPT: '0',
  };
}

// ── Project history (cross-screenshot memory) ──

const MAX_HISTORY_ENTRIES = 20;

function historyFile(projectPath) {
  const slug = path.basename(projectPath);
  return path.join(HISTORY_DIR, `${slug}.json`);
}

function loadHistory(projectPath) {
  try {
    return JSON.parse(fs.readFileSync(historyFile(projectPath), 'utf8'));
  } catch {
    return [];
  }
}

function appendHistory(projectPath, entry) {
  fs.mkdirSync(HISTORY_DIR, { recursive: true });
  const history = loadHistory(projectPath);
  history.push(entry);
  // Keep only the most recent entries
  const trimmed = history.slice(-MAX_HISTORY_ENTRIES);
  fs.writeFileSync(historyFile(projectPath), JSON.stringify(trimmed, null, 2) + '\n');
}

function formatHistoryForPrompt(history) {
  if (!history.length) return '';
  let text = '\n\n## Previous fixes in this project\n';
  for (const entry of history) {
    text += `\n### ${entry.date}\n`;
    if (entry.userMessage) text += `User: ${entry.userMessage}\n`;
    text += `Summary: ${entry.summary}\n`;
    if (entry.filesChanged && entry.filesChanged.length) {
      text += `Files: ${entry.filesChanged.join(', ')}\n`;
    }
  }
  text += '\nUse this history as context — you may be fixing related issues or continuing prior work.\n';
  return text;
}

function mediaType(filePath) {
  const ext = path.extname(filePath).toLowerCase();
  const types = { '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.webp': 'image/webp', '.gif': 'image/gif' };
  return types[ext] || 'image/png';
}

function repoURL(slug) {
  if (slug.startsWith('http') || slug.startsWith('git@')) return slug;
  const token = getGhToken();
  if (token) return `https://x-access-token:${token}@github.com/${slug}.git`;
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

// Call Claude API with a messages array, return the response + updated messages
async function callClaude(messages) {
  const apiKey = getApiKey();
  if (!apiKey) throw new Error('No API key. Set ANTHROPIC_API_KEY or save to ~/.xmuggle/api-key');

  const body = {
    model: getModel(),
    max_tokens: MAX_TOKENS,
    system: SYSTEM_PROMPT,
    tools: [EDIT_FILE_TOOL],
    tool_choice: { type: 'auto' },
    messages,
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
    const err = await resp.text();
    throw new Error(`API error ${resp.status}: ${err}`);
  }

  return await resp.json();
}

async function analyzeAndFix({ imagePaths, projectPath, message, onProgress, priorMessages }) {
  const log = onProgress || (() => {});
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

  log(`Cloning ${repo} (shallow)…`);
  try {
    execSync(`git clone --depth 1 ${repoURL(repo)} "${cloneDir}"`, { encoding: 'utf8', stdio: 'pipe', env: gitEnv() });
  } catch (e) {
    throw new Error(`Clone failed: ${e.stderr || e.message}`);
  }
  log('Clone complete');

  log('On main branch…');

  // Build messages array
  let messages;
  if (priorMessages && priorMessages.length > 0) {
    // Continue conversation — ensure proper tool call/result pairing
    messages = [...priorMessages];
    
    // Check if the last assistant message has tool calls that need results
    const lastMessage = messages[messages.length - 1];
    if (lastMessage && lastMessage.role === 'assistant' && Array.isArray(lastMessage.content)) {
      const toolCalls = lastMessage.content.filter(block => block.type === 'tool_use');
      if (toolCalls.length > 0) {
        // Add tool results for any previous tool calls
        const toolResults = toolCalls.map(toolCall => ({
          type: 'tool_result',
          tool_use_id: toolCall.id,
          content: 'Tool execution completed successfully.'
        }));
        
        messages.push({
          role: 'user',
          content: toolResults
        });
      }
    }
    
    // Add the new user message
    messages.push({ 
      role: 'user', 
      content: [{ type: 'text', text: message || 'Please continue fixing.' }] 
    });
    
    log('Continuing conversation…');
  } else {
    // First message — include images + repo context
    log(`Encoding ${imagePaths.length} image(s)…`);
    const imageBlocks = imagePaths.map(p => ({
      type: 'image',
      source: {
        type: 'base64',
        media_type: mediaType(p),
        data: fs.readFileSync(p).toString('base64'),
      },
    }));

    log('Gathering repo context (file list + source files)…');
    const ctx = getRepoContext(cloneDir);
    log(`Found ${ctx.files.length} tracked files, reading ${ctx.fileContents.length} source files`);
    let contextText = `Repository: ${repo}\n\nFiles in repo:\n${ctx.files.join('\n')}\n\n`;

    for (const f of ctx.fileContents) {
      contextText += `--- ${f.path} ---\n${f.content}\n\n`;
    }

    // Include project history for cross-screenshot memory
    const history = loadHistory(projectPath);
    if (history.length) {
      log(`Including ${history.length} previous fix(es) as context`);
      contextText += formatHistoryForPrompt(history);
    }

    if (message) {
      contextText += `\nUser context: ${message}\n`;
    }

    contextText += '\nAnalyze the screenshot(s) and fix the issue using the edit_file tool.';

    messages = [{
      role: 'user',
      content: [...imageBlocks, { type: 'text', text: contextText }],
    }];
  }

  // Call API
  log(`Calling Claude API (${getModel()})…`);
  const result = await callClaude(messages);

  log('API response received, parsing…');

  // Extract edits and text from response
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

  // Build conversation history: append assistant response
  const updatedMessages = [...messages, { role: 'assistant', content: result.content }];

  // Build display-friendly conversation
  const conversation = buildConversation(updatedMessages);

  if (edits.length === 0) {
    log('No edits returned — cleaning up');
    appendHistory(projectPath, {
      date: new Date().toISOString(),
      userMessage: message || '',
      summary: summary || 'No changes needed.',
      filesChanged: [],
    });
    fs.rmSync(cloneDir, { recursive: true, force: true });
    return { status: 'no_changes', summary: summary || 'No changes needed.', messages: updatedMessages, conversation };
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

  // Commit and push directly to main
  const commitSummary = edits.map(e => e.summary).join('; ');
  const commitMsg = `fix: ${commitSummary}`;

  try {
    log('Staging and committing to main…');
    for (const f of changedFiles) {
      execSync(`git add -- "${f}"`, { cwd: cloneDir, stdio: 'pipe' });
    }
    execSync(`git commit -m "${commitMsg.replace(/"/g, '\\"')}"`, { cwd: cloneDir, stdio: 'pipe' });

    log('Pushing to main…');
    execSync('git push origin main', { cwd: cloneDir, stdio: 'pipe', env: gitEnv() });
    log('Pushed to main');
  } catch (e) {
    log(`Push failed: ${e.stderr || e.message}`);
    fs.rmSync(cloneDir, { recursive: true, force: true });
    return {
      status: 'push_failed',
      summary: `Edits applied but push failed: ${e.stderr || e.message}`,
      changedFiles,
      messages: updatedMessages,
      conversation,
    };
  }

  // Save to project history for future context
  appendHistory(projectPath, {
    date: new Date().toISOString(),
    userMessage: message || '',
    summary: commitSummary,
    filesChanged: changedFiles,
  });

  // Clean up clone
  log('Cleaning up temp clone…');
  fs.rmSync(cloneDir, { recursive: true, force: true });
  log('Done!');

  return {
    status: 'success',
    summary: commitSummary,
    changedFiles,
    analysisText: summary,
    messages: updatedMessages,
    conversation,
  };
}

// Convert raw API messages to display-friendly format
function buildConversation(messages) {
  const conv = [];
  for (const msg of messages) {
    if (msg.role === 'user') {
      // Extract user text (skip images, repo context, and tool results)
      let text = '';
      if (Array.isArray(msg.content)) {
        for (const block of msg.content) {
          if (block.type === 'text') {
            // Pull out just the "User context:" line if present
            const match = block.text.match(/User context:\s*(.+)/);
            if (match) {
              text = match[1].trim();
            } else if (!block.text.startsWith('Repository:')) {
              text = block.text;
            }
          }
          // Skip tool_result blocks in conversation display
        }
      } else {
        text = msg.content;
      }
      if (text) conv.push({ role: 'user', text });
    } else if (msg.role === 'assistant') {
      let text = '';
      const files = [];
      if (Array.isArray(msg.content)) {
        for (const block of msg.content) {
          if (block.type === 'text') text += block.text;
          if (block.type === 'tool_use' && block.name === 'edit_file') {
            files.push(block.input.path + ': ' + block.input.summary);
          }
        }
      }
      const parts = [];
      if (text) parts.push(text);
      if (files.length) parts.push('Files changed:\n' + files.map(f => '  - ' + f).join('\n'));
      if (parts.length) conv.push({ role: 'assistant', text: parts.join('\n\n') });
    }
  }
  return conv;
}

module.exports = { getApiKey, setApiKey, resetApiKey, hasApiKey, getGhToken, setGhToken, resetGhToken, hasGhToken, getModel, setModel, listModels, analyzeAndFix };