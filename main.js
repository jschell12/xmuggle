const { app, BrowserWindow, ipcMain, shell } = require('electron');
const path = require('path');
const fs = require('fs');
const os = require('os');
const { execSync } = require('child_process');

const DESKTOP = path.join(os.homedir(), 'Desktop');
const IMAGE_EXTS = new Set(['.png', '.jpg', '.jpeg', '.webp', '.gif']);

let repoRoot = null;
let xmuggleDir = null;

function findRepoRoot() {
  try {
    return execSync('git rev-parse --show-toplevel', { cwd: __dirname, encoding: 'utf8' }).trim();
  } catch {
    return path.resolve(__dirname, '..');
  }
}

function getDesktopImages() {
  try {
    const entries = fs.readdirSync(DESKTOP, { withFileTypes: true });
    const images = [];
    for (const e of entries) {
      if (e.name.startsWith('.')) continue;
      const ext = path.extname(e.name).toLowerCase();
      if (!IMAGE_EXTS.has(ext)) continue;
      const full = path.join(DESKTOP, e.name);
      try {
        const stat = fs.statSync(full);
        images.push({ path: full, name: e.name, mtime: stat.mtimeMs });
      } catch {}
    }
    images.sort((a, b) => b.mtime - a.mtime);
    return images;
  } catch {
    return [];
  }
}

function readJSON(filePath) {
  try {
    return JSON.parse(fs.readFileSync(filePath, 'utf8'));
  } catch {
    return null;
  }
}

function getTaskStatus() {
  const queueDir = path.join(xmuggleDir, 'queue');
  const resultsDir = path.join(xmuggleDir, 'results');
  const tasks = {};

  // Read all tasks
  try {
    for (const taskId of fs.readdirSync(queueDir)) {
      if (taskId.startsWith('.')) continue;
      const taskDir = path.join(queueDir, taskId);
      const task = readJSON(path.join(taskDir, 'task.json'));
      if (!task) continue;

      // Find screenshot filenames in task dir
      try {
        for (const f of fs.readdirSync(taskDir)) {
          if (f.startsWith('screenshot')) {
            tasks[taskId] = { status: task.status, file: f, taskId };
          }
        }
      } catch {}
    }
  } catch {}

  // Check for results
  try {
    for (const taskId of fs.readdirSync(resultsDir)) {
      if (taskId.startsWith('.')) continue;
      const result = readJSON(path.join(resultsDir, taskId, 'result.json'));
      if (!result) continue;
      if (tasks[taskId]) {
        tasks[taskId].result = result;
      } else {
        tasks[taskId] = { status: 'done', taskId, result };
      }
    }
  } catch {}

  return tasks;
}

function getImages() {
  const desktopImages = getDesktopImages();
  const imagesIndex = readJSON(path.join(xmuggleDir, 'images.json')) || { images: {} };
  const tasks = getTaskStatus();

  // Build a map: image path → task info
  // Match by checking if any task's queue dir contains a screenshot that was copied from this image
  const tasksByPath = {};
  for (const [taskId, info] of Object.entries(tasks)) {
    // The image path is tracked in images.json; match by checking task creation time vs image
    tasksByPath[taskId] = info;
  }

  return desktopImages.map(img => {
    const tracked = imagesIndex.images?.[img.path];
    let status = 'new';

    if (tracked && tracked.status === 'done') {
      status = 'done';
    } else if (tracked && tracked.status === 'pending') {
      // Check if there's a task for this image
      let found = false;
      for (const [, info] of Object.entries(tasks)) {
        if (info.result) {
          if (info.result.status === 'success') { status = 'done'; found = true; break; }
          if (info.result.status === 'error') { status = 'error'; found = true; break; }
        }
        if (info.status === 'processing') { status = 'processing'; found = true; break; }
        if (info.status === 'pending') { status = 'queued'; found = true; break; }
      }
      if (!found) status = 'pending';
    }

    return { ...img, status };
  });
}

function createWindow() {
  const win = new BrowserWindow({
    width: 1200,
    height: 800,
    title: 'xmuggle',
    backgroundColor: '#1a1a2e',
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
    },
  });

  win.loadFile(path.join(__dirname, 'renderer', 'index.html'));

  // Watch for changes and notify renderer
  const watchDirs = [DESKTOP, xmuggleDir];
  for (const dir of watchDirs) {
    try {
      fs.watch(dir, { recursive: true }, () => {
        try {
          win.webContents.send('images-updated', getImages());
        } catch {}
      });
    } catch {}
  }
}

app.whenReady().then(() => {
  repoRoot = findRepoRoot();
  xmuggleDir = path.join(repoRoot, '.xmuggle');

  const api = require('./api');

  ipcMain.handle('get-images', () => getImages());
  ipcMain.handle('delete-image', (_, imgPath) => {
    try { fs.unlinkSync(imgPath); } catch {}
    return getImages();
  });
  ipcMain.handle('has-api-key', () => api.hasApiKey());
  ipcMain.handle('set-api-key', (_, key) => { api.setApiKey(key); return true; });
  ipcMain.handle('reset-api-key', () => { api.resetApiKey(); return true; });
  ipcMain.handle('open-external', (_, url) => shell.openExternal(url));
  ipcMain.handle('send-to-api', async (_, imagePaths, message) => {
    return api.analyzeAndFix({ imagePaths, repoRoot, message });
  });

  createWindow();
});

app.on('window-all-closed', () => app.quit());
