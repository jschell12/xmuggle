const { app, BrowserWindow, ipcMain, shell, dialog, powerMonitor } = require('electron');
const http = require('http');
const { execSync, exec } = require('child_process');
const path = require('path');
const fs = require('fs');
const os = require('os');

const DESKTOP = path.join(os.homedir(), 'Desktop');
const XMUGGLE_DIR = path.join(os.homedir(), '.xmuggle');
const PROJECTS_FILE = path.join(XMUGGLE_DIR, 'projects.json');
const TASKS_FILE = path.join(XMUGGLE_DIR, 'tasks.json');
const INBOX_DIR = path.join(XMUGGLE_DIR, 'inbox');
const NOTES_DIR = path.join(XMUGGLE_DIR, 'notes');
const QUEUE_REPO_DIR = path.join(XMUGGLE_DIR, 'queue-repo');
const QUEUE_CONF_FILE = path.join(XMUGGLE_DIR, 'queue-url');
const PID_FILE = path.join(XMUGGLE_DIR, 'daemon.pid');
const DAEMON_LOG = path.join(XMUGGLE_DIR, 'daemon.log');
const DAEMON_BIN = path.join(os.homedir(), '.local', 'bin', 'xmuggled');
const SERVER_PORT = 24816;
let relayServer = null;
const IMAGE_EXTS = new Set(['.png', '.jpg', '.jpeg', '.webp', '.gif']);
const TEXT_EXTS = new Set(['.txt', '.md']);
const TEXT_PREVIEW_CHARS = 400;

// ── Projects ──

function loadProjects() {
  try {
    const data = JSON.parse(fs.readFileSync(PROJECTS_FILE, 'utf8'));
    // Migrate legacy string entries to objects
    return data.map(p => typeof p === 'string' ? { path: p, gitUrl: '' } : p);
  } catch { return []; }
}

function saveProjects(list) {
  fs.mkdirSync(XMUGGLE_DIR, { recursive: true });
  fs.writeFileSync(PROJECTS_FILE, JSON.stringify(list, null, 2) + '\n');
}

function addProject(dirPath, gitUrl) {
  const abs = path.resolve(dirPath);
  if (!fs.existsSync(path.join(abs, '.git'))) return { error: 'Not a git repo' };

  // Auto-detect git remote if not provided
  if (!gitUrl) {
    try {
      gitUrl = execSync('git remote get-url origin', { cwd: abs, encoding: 'utf8', stdio: 'pipe' }).trim();
    } catch {
      gitUrl = '';
    }
  }

  const projects = loadProjects();
  const existing = projects.find(p => p.path === abs);
  if (existing) {
    existing.gitUrl = gitUrl || existing.gitUrl;
  } else {
    projects.push({ path: abs, gitUrl });
  }
  saveProjects(projects);
  return { path: abs, name: path.basename(abs), gitUrl };
}

function removeProject(dirPath) {
  const projects = loadProjects().filter(p => p.path !== dirPath);
  saveProjects(projects);
}

function listProjects() {
  return loadProjects().map(p => ({ path: p.path, name: path.basename(p.path), gitUrl: p.gitUrl || '' }));
}

// ── Tasks ──

function loadTasks() {
  try { return JSON.parse(fs.readFileSync(TASKS_FILE, 'utf8')); } catch { return {}; }
}

function saveTasks(tasks) {
  fs.mkdirSync(XMUGGLE_DIR, { recursive: true });
  fs.writeFileSync(TASKS_FILE, JSON.stringify(tasks, null, 2) + '\n');
}

function updateTaskStatus(imagePath, updates) {
  const tasks = loadTasks();
  const existing = tasks[imagePath] || {};
  tasks[imagePath] = { ...existing, ...updates };
  saveTasks(tasks);
}

// Sync task statuses from queue repo
function syncTaskStatuses() {
  const tasks = loadTasks();
  let changed = false;

  // Pull queue repo for latest status updates
  if (fs.existsSync(path.join(QUEUE_REPO_DIR, '.git'))) {
    const api = require('./api');
    const env = api.gitEnv();
    try {
      execSync('git pull --rebase origin main', { cwd: QUEUE_REPO_DIR, stdio: 'pipe', env });
    } catch {}
  }

  // Check queue repo meta.json for status changes
  const pendingDir = path.join(QUEUE_REPO_DIR, 'pending');
  for (const [imgPath, task] of Object.entries(tasks)) {
    if (task.status === 'done' || task.status === 'error') continue;

    // Match by queueTaskId if available
    if (task.queueTaskId) {
      const metaFile = path.join(pendingDir, task.queueTaskId, 'meta.json');
      try {
        const meta = JSON.parse(fs.readFileSync(metaFile, 'utf8'));
        if (meta.status && meta.status !== task.status) {
          task.status = meta.status;
          task.result = meta.result || '';
          task.processedBy = meta.processedBy || '';
          task.doneAt = meta.doneAt || '';
          changed = true;
        }
      } catch {}
      continue;
    }

    // Fallback: scan queue for tasks with matching filename
    const imgName = path.basename(imgPath);
    try {
      const dirs = fs.readdirSync(pendingDir);
      for (const dir of dirs) {
        const metaFile = path.join(pendingDir, dir, 'meta.json');
        try {
          const meta = JSON.parse(fs.readFileSync(metaFile, 'utf8'));
          if (meta.filenames && meta.filenames.includes(imgName) && meta.status && meta.status !== task.status) {
            task.queueTaskId = dir;
            task.status = meta.status;
            task.result = meta.result || '';
            task.processedBy = meta.processedBy || '';
            task.doneAt = meta.doneAt || '';
            changed = true;
            break;
          }
        } catch {}
      }
    } catch {}
  }

  if (changed) saveTasks(tasks);
  return tasks;
}

// ── Items (images + text notes) ──

function readTextPreview(full) {
  try {
    const raw = fs.readFileSync(full, 'utf8');
    return raw.length > TEXT_PREVIEW_CHARS ? raw.slice(0, TEXT_PREVIEW_CHARS) + '\u2026' : raw;
  } catch {
    return '';
  }
}

function scanDir(dir, tasks, { allowText = false } = {}) {
  const items = [];
  try {
    const entries = fs.readdirSync(dir);
    for (const name of entries) {
      if (name.startsWith('.')) continue;
      const ext = path.extname(name).toLowerCase();
      const isImage = IMAGE_EXTS.has(ext);
      const isText = allowText && TEXT_EXTS.has(ext);
      if (!isImage && !isText) continue;
      const full = path.join(dir, name);
      try {
        const stat = fs.statSync(full);
        const task = tasks[full];
        const status = task ? task.status : 'new';
        const projectPath = task ? task.projectPath : '';
        const conversation = task ? (task.conversation || []) : [];
        const result = task ? (task.result || '') : '';
        const base = { path: full, name, mtime: stat.mtimeMs, status, projectPath, conversation, result };
        if (isImage) {
          items.push({ ...base, type: 'image' });
        } else {
          items.push({ ...base, type: 'text', preview: readTextPreview(full) });
        }
      } catch {}
    }
  } catch {}
  return items;
}

function getDesktopImages() {
  const tasks = syncTaskStatuses();
  const items = [
    ...scanDir(DESKTOP, tasks),
    ...scanDir(INBOX_DIR, tasks, { allowText: true }),
    ...scanDir(NOTES_DIR, tasks, { allowText: true }),
  ];
  items.sort((a, b) => b.mtime - a.mtime);
  return items;
}

function createNote(text) {
  fs.mkdirSync(NOTES_DIR, { recursive: true });
  const id = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
  const name = `note-${id}.txt`;
  const full = path.join(NOTES_DIR, name);
  fs.writeFileSync(full, text);
  return { path: full, name };
}

// ── Queue repo sync ──

function getQueueUrl() {
  try { return fs.readFileSync(QUEUE_CONF_FILE, 'utf8').trim(); } catch { return ''; }
}

function setQueueUrl(url) {
  fs.mkdirSync(XMUGGLE_DIR, { recursive: true });
  fs.writeFileSync(QUEUE_CONF_FILE, url.trim() + '\n');
}

function ensureQueueClone() {
  const url = getQueueUrl();
  if (!url) return null;

  const api = require('./api');
  const env = api.gitEnv();

  if (!fs.existsSync(path.join(QUEUE_REPO_DIR, '.git'))) {
    fs.mkdirSync(QUEUE_REPO_DIR, { recursive: true });
    try {
      execSync(`git clone "${url}" "${QUEUE_REPO_DIR}"`, { stdio: 'pipe', env });
    } catch (e) {
      console.error('Queue clone failed:', e.message);
      return null;
    }
  } else {
    try {
      execSync('git pull --rebase origin main', { cwd: QUEUE_REPO_DIR, stdio: 'pipe', env });
    } catch {}
  }
  return QUEUE_REPO_DIR;
}

function getProjectSlug(projectPath) {
  // Get "user/repo" from git remote, fall back to dir name
  try {
    const remote = execSync('git remote get-url origin', { cwd: projectPath, encoding: 'utf8', stdio: 'pipe' }).trim();
    return remote.replace(/^https:\/\/github\.com\//, '').replace(/^git@github\.com:/, '').replace(/\.git$/, '');
  } catch {
    return path.basename(projectPath);
  }
}

function queuePush(imagePaths, projectPath, message) {
  const queueDir = ensureQueueClone();
  if (!queueDir) throw new Error('No queue repo configured. Set it in the relay dropdown.');

  const api = require('./api');
  const env = api.gitEnv();
  const crypto = require('crypto');
  const id = Date.now().toString(36) + '-' + crypto.randomBytes(3).toString('hex');
  const taskDir = path.join(queueDir, 'pending', id);
  fs.mkdirSync(taskDir, { recursive: true });

  const filenames = [];
  for (const imgPath of imagePaths) {
    const filename = path.basename(imgPath);
    fs.copyFileSync(imgPath, path.join(taskDir, filename));
    filenames.push(filename);
  }

  const project = getProjectSlug(projectPath);

  fs.writeFileSync(path.join(taskDir, 'meta.json'), JSON.stringify({
    filenames,
    project,
    message: message || '',
    from: os.hostname(),
    timestamp: new Date().toISOString(),
    status: 'pending',
  }, null, 2) + '\n');

  execSync('git add -A', { cwd: queueDir, stdio: 'pipe' });
  const commitMsg = `xmuggle: ${project} — ${filenames.join(', ')}`;
  execSync(`git commit -m "${commitMsg.replace(/"/g, '\\"')}"`, { cwd: queueDir, stdio: 'pipe' });
  execSync('git push', { cwd: queueDir, stdio: 'pipe', env });

  return { status: 'queued', id, project };
}

// ── Daemon control ──

function getDaemonStatus() {
  try {
    const pid = fs.readFileSync(PID_FILE, 'utf8').trim();
    if (!pid) return { running: false };
    // Check if process is alive
    try {
      process.kill(Number(pid), 0);
      return { running: true, pid: Number(pid) };
    } catch {
      return { running: false };
    }
  } catch {
    return { running: false };
  }
}

function daemonStart() {
  const status = getDaemonStatus();
  if (status.running) return { ok: true, message: 'Already running', pid: status.pid };

  if (!fs.existsSync(DAEMON_BIN)) {
    return { ok: false, message: 'Daemon binary not found. Run "make install" first.' };
  }

  try {
    // Clean stale PID file
    try { fs.unlinkSync(PID_FILE); } catch {}
    execSync(`"${DAEMON_BIN}" start`, { stdio: 'pipe', timeout: 5000 });
    // Give it a moment to write PID
    const after = getDaemonStatus();
    return { ok: true, message: 'Started', pid: after.pid || null };
  } catch (e) {
    return { ok: false, message: e.message };
  }
}

function daemonStop() {
  const status = getDaemonStatus();
  if (!status.running) return { ok: true, message: 'Not running' };

  try {
    execSync(`"${DAEMON_BIN}" stop`, { stdio: 'pipe', timeout: 5000 });
    return { ok: true, message: 'Stopped' };
  } catch (e) {
    // Fallback: kill directly
    try {
      process.kill(status.pid, 'SIGTERM');
      try { fs.unlinkSync(PID_FILE); } catch {}
      return { ok: true, message: 'Stopped (fallback)' };
    } catch {
      return { ok: false, message: e.message };
    }
  }
}

function getDaemonLog(lines) {
  try {
    const content = fs.readFileSync(DAEMON_LOG, 'utf8');
    const all = content.split('\n');
    return all.slice(-lines).join('\n');
  } catch {
    return '';
  }
}

// ── Window ──

function createWindow() {
  const win = new BrowserWindow({
    width: 1200,
    height: 800,
    title: 'xmuggle',
    backgroundColor: '#1a1a2e',
    icon: path.join(__dirname, 'assets', 'icon.png'),
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
    },
  });

  win.loadFile(path.join(__dirname, 'renderer', 'index.html'));

  // Watch Desktop
  try {
    fs.watch(DESKTOP, () => {
      try { win.webContents.send('images-updated', getDesktopImages()); } catch {}
    });
  } catch {}

  // Watch each project's .xmuggle/ dir
  for (const p of loadProjects()) {
    const xdir = path.join(p.path, '.xmuggle');
    try {
      fs.watch(xdir, { recursive: true }, () => {
        try { win.webContents.send('images-updated', getDesktopImages()); } catch {}
      });
    } catch {}
  }

  return win;
}

app.whenReady().then(() => {
  if (process.platform === 'darwin' && app.dock) {
    app.dock.setIcon(path.join(__dirname, 'assets', 'icon.png'));
  }
  const api = require('./api');

  ipcMain.handle('get-images', () => getDesktopImages());
  ipcMain.handle('delete-image', (_, imgPath) => {
    try { fs.unlinkSync(imgPath); } catch {}
    // Also remove from tasks
    const tasks = loadTasks();
    delete tasks[imgPath];
    saveTasks(tasks);
    return getDesktopImages();
  });

  // Projects
  ipcMain.handle('list-projects', () => listProjects());
  ipcMain.handle('add-project', async (_, gitUrl, dirPath) => {
    if (dirPath) {
      return addProject(dirPath, gitUrl);
    }
    const result = await dialog.showOpenDialog({ properties: ['openDirectory'] });
    if (result.canceled || !result.filePaths.length) return null;
    return addProject(result.filePaths[0], gitUrl);
  });
  ipcMain.handle('remove-project', (_, dirPath) => {
    removeProject(dirPath);
    return listProjects();
  });

  // GitHub token
  ipcMain.handle('has-gh-token', () => api.hasGhToken());
  ipcMain.handle('set-gh-token', (_, token) => { api.setGhToken(token); return true; });
  ipcMain.handle('reset-gh-token', () => { api.resetGhToken(); return true; });

  ipcMain.handle('open-external', (_, url) => shell.openExternal(url));

  // Daemon control
  ipcMain.handle('daemon-status', () => getDaemonStatus());
  ipcMain.handle('daemon-start', () => daemonStart());
  ipcMain.handle('daemon-stop', () => daemonStop());
  ipcMain.handle('daemon-log', (_, lines) => getDaemonLog(lines || 50));

  // Daemon config (repos + postCommands)
  const DAEMON_CONFIG_FILE = path.join(XMUGGLE_DIR, 'daemon.json');
  ipcMain.handle('get-daemon-config', () => {
    try { return JSON.parse(fs.readFileSync(DAEMON_CONFIG_FILE, 'utf8')); } catch { return {}; }
  });
  ipcMain.handle('set-daemon-config', (_, cfg) => {
    fs.mkdirSync(XMUGGLE_DIR, { recursive: true });
    fs.writeFileSync(DAEMON_CONFIG_FILE, JSON.stringify(cfg, null, 2) + '\n');
    return true;
  });

  // Relay
  ipcMain.handle('get-relay-host', () => api.getRelayHost());
  ipcMain.handle('set-relay-host', (_, host) => { api.setRelayHost(host); return true; });

  // Scan local network for xmuggle relay servers
  ipcMain.handle('scan-network', async () => {
    const nets = os.networkInterfaces();
    const localIPs = [];
    for (const iface of Object.values(nets)) {
      for (const cfg of iface) {
        if (cfg.family === 'IPv4' && !cfg.internal) localIPs.push(cfg.address);
      }
    }
    if (!localIPs.length) return [];

    const myIP = localIPs[0];
    const subnet = myIP.split('.').slice(0, 3).join('.');
    const found = [];

    const probe = async (ip) => {
      if (ip === myIP) return; // skip self
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), 500);
      try {
        const resp = await fetch(`http://${ip}:${SERVER_PORT}/status`, { signal: controller.signal });
        if (resp.ok) {
          const data = await resp.json();
          found.push({ ip, hostname: data.hostname || ip, projects: data.projects || [] });
        }
      } catch {} finally { clearTimeout(timer); }
    };

    // Scan 1-254 in parallel batches
    const ips = [];
    for (let i = 1; i <= 254; i++) ips.push(`${subnet}.${i}`);
    const batchSize = 50;
    for (let i = 0; i < ips.length; i += batchSize) {
      await Promise.all(ips.slice(i, i + batchSize).map(probe));
    }
    return found;
  });
  ipcMain.handle('send-to-relay', async (_, imagePath, project, message) => {
    const host = api.getRelayHost();
    if (!host) throw new Error('No relay host configured');
    const imageData = fs.readFileSync(imagePath).toString('base64');
    const filename = path.basename(imagePath);
    const body = JSON.stringify({ image: imageData, filename, project, message });
    const resp = await fetch(`http://${host}:${SERVER_PORT}/submit`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body,
    });
    if (!resp.ok) {
      const err = await resp.text();
      throw new Error(`Relay error ${resp.status}: ${err}`);
    }
    return await resp.json();
  });

  // Queue repo
  ipcMain.handle('get-queue-url', () => getQueueUrl());
  ipcMain.handle('set-queue-url', (_, url) => { setQueueUrl(url); return true; });
  ipcMain.handle('queue-push', (_, imagePaths, projectPath, message) => {
    const result = queuePush(imagePaths, projectPath, message);
    // Track the queue task ID locally so we can poll for status
    const imgPath = imagePaths[0];
    updateTaskStatus(imgPath, {
      projectPath,
      queueTaskId: result.id,
      status: 'queued',
      message: message || '',
    });
    return result;
  });

  // Create a pasted-text note item.
  ipcMain.handle('create-note', (_, text) => {
    if (!text || !text.trim()) throw new Error('Empty note');
    return createNote(text);
  });

  // Save item with project and message (no API call)
  ipcMain.handle('save-item', (_, imagePath, projectPath, message) => {
    const taskId = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
    const conversation = message ? [{ role: 'user', text: message }] : [];
    updateTaskStatus(imagePath, { projectPath, taskId, status: 'pending', conversation });
    return { status: 'saved' };
  });

  const win = createWindow();

  // ── Relay server: receive images from remote xmuggle instances ──
  fs.mkdirSync(INBOX_DIR, { recursive: true });
  fs.mkdirSync(NOTES_DIR, { recursive: true });

  // Watch inbox + notes for new items
  try {
    fs.watch(INBOX_DIR, () => {
      try { win.webContents.send('images-updated', getDesktopImages()); } catch {}
    });
  } catch {}
  try {
    fs.watch(NOTES_DIR, () => {
      try { win.webContents.send('images-updated', getDesktopImages()); } catch {}
    });
  } catch {}

  relayServer = http.createServer((req, res) => {
    // CORS
    res.setHeader('Access-Control-Allow-Origin', '*');
    res.setHeader('Access-Control-Allow-Methods', 'POST, GET, OPTIONS');
    res.setHeader('Access-Control-Allow-Headers', 'Content-Type');
    if (req.method === 'OPTIONS') { res.writeHead(200); res.end(); return; }

    if (req.method === 'GET' && req.url === '/status') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ status: 'ok', hostname: os.hostname(), projects: listProjects() }));
      return;
    }

    if (req.method === 'POST' && req.url === '/submit') {
      let body = [];
      req.on('data', chunk => body.push(chunk));
      req.on('end', () => {
        try {
          const data = JSON.parse(Buffer.concat(body).toString());
          const { image, filename, project, message } = data;
          if (!image || !filename) {
            res.writeHead(400, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ error: 'image and filename required' }));
            return;
          }

          // Save image to inbox
          const imgPath = path.join(INBOX_DIR, filename);
          fs.writeFileSync(imgPath, Buffer.from(image, 'base64'));

          // Pre-assign project and message if provided
          if (project) {
            const taskId = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
            updateTaskStatus(imgPath, {
              projectPath: project, taskId, status: 'new',
              conversation: [{ role: 'user', text: message || '' }],
            });
          }

          // Save the message as a sidecar so the UI can show it
          if (message) {
            fs.writeFileSync(imgPath + '.msg', message);
          }

          try { win.webContents.send('images-updated', getDesktopImages()); } catch {}

          res.writeHead(200, { 'Content-Type': 'application/json' });
          res.end(JSON.stringify({ status: 'received', path: imgPath }));
        } catch (e) {
          res.writeHead(400, { 'Content-Type': 'application/json' });
          res.end(JSON.stringify({ error: e.message }));
        }
      });
      return;
    }

    res.writeHead(404);
    res.end('Not found');
  });

  relayServer.on('error', (err) => {
    if (err.code === 'EADDRINUSE') {
      console.error(`Port ${SERVER_PORT} already in use — relay server disabled`);
    } else {
      console.error('Relay server error:', err.message);
    }
  });

  relayServer.listen(SERVER_PORT, '0.0.0.0', () => {
    console.log(`xmuggle relay server listening on port ${SERVER_PORT}`);
  });

  // Poll queue repo for task status updates every 10s
  setInterval(() => {
    try {
      const images = getDesktopImages(); // calls syncTaskStatuses internally
      win.webContents.send('images-updated', images);
    } catch {}
  }, 10_000);

  // Immediately sync when system wakes from sleep
  powerMonitor.on('resume', () => {
    try {
      const images = getDesktopImages();
      win.webContents.send('images-updated', images);
    } catch {}
  });

});

app.on('window-all-closed', () => {
  if (relayServer) {
    relayServer.close(() => app.quit());
  } else {
    app.quit();
  }
});
