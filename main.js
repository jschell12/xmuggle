const { app, BrowserWindow, ipcMain, shell, dialog } = require('electron');
const path = require('path');
const fs = require('fs');
const os = require('os');

const DESKTOP = path.join(os.homedir(), 'Desktop');
const XMUGGLE_DIR = path.join(os.homedir(), '.xmuggle');
const PROJECTS_FILE = path.join(XMUGGLE_DIR, 'projects.json');
const TASKS_FILE = path.join(XMUGGLE_DIR, 'tasks.json');
const IMAGE_EXTS = new Set(['.png', '.jpg', '.jpeg', '.webp', '.gif']);

// ── Projects ──

function loadProjects() {
  try { return JSON.parse(fs.readFileSync(PROJECTS_FILE, 'utf8')); } catch { return []; }
}

function saveProjects(list) {
  fs.mkdirSync(XMUGGLE_DIR, { recursive: true });
  fs.writeFileSync(PROJECTS_FILE, JSON.stringify(list, null, 2) + '\n');
}

function addProject(dirPath) {
  const abs = path.resolve(dirPath);
  if (!fs.existsSync(path.join(abs, '.git'))) return { error: 'Not a git repo' };
  const projects = loadProjects();
  if (!projects.includes(abs)) {
    projects.push(abs);
    saveProjects(projects);
  }
  return { path: abs, name: path.basename(abs) };
}

function removeProject(dirPath) {
  const projects = loadProjects().filter(p => p !== dirPath);
  saveProjects(projects);
}

function listProjects() {
  return loadProjects().map(p => ({ path: p, name: path.basename(p) }));
}

// ── Tasks ──

function loadTasks() {
  try { return JSON.parse(fs.readFileSync(TASKS_FILE, 'utf8')); } catch { return {}; }
}

function saveTasks(tasks) {
  fs.mkdirSync(XMUGGLE_DIR, { recursive: true });
  fs.writeFileSync(TASKS_FILE, JSON.stringify(tasks, null, 2) + '\n');
}

function updateTaskStatus(imagePath, projectPath, taskId, status, prUrl) {
  const tasks = loadTasks();
  tasks[imagePath] = { projectPath, taskId, status, prUrl: prUrl || '' };
  saveTasks(tasks);
}

// Check project results dirs to update task statuses
function syncTaskStatuses() {
  const tasks = loadTasks();
  let changed = false;
  for (const [imgPath, task] of Object.entries(tasks)) {
    if (task.status === 'done' || task.status === 'error') continue;
    if (!task.projectPath || !task.taskId) continue;
    const resultFile = path.join(task.projectPath, '.xmuggle', 'results', task.taskId, 'result.json');
    try {
      const result = JSON.parse(fs.readFileSync(resultFile, 'utf8'));
      task.status = result.status === 'success' ? 'done' : 'error';
      task.prUrl = result.pr_url || '';
      changed = true;
    } catch {}
  }
  if (changed) saveTasks(tasks);
  return tasks;
}

// ── Images ──

function getDesktopImages() {
  const tasks = syncTaskStatuses();
  try {
    const entries = fs.readdirSync(DESKTOP);
    const images = [];
    for (const name of entries) {
      if (name.startsWith('.')) continue;
      const ext = path.extname(name).toLowerCase();
      if (!IMAGE_EXTS.has(ext)) continue;
      const full = path.join(DESKTOP, name);
      try {
        const stat = fs.statSync(full);
        const task = tasks[full];
        const status = task ? task.status : 'new';
        const projectPath = task ? task.projectPath : '';
        images.push({ path: full, name, mtime: stat.mtimeMs, status, projectPath });
      } catch {}
    }
    images.sort((a, b) => b.mtime - a.mtime);
    return images;
  } catch {
    return [];
  }
}

// ── Window ──

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

  // Watch Desktop
  try {
    fs.watch(DESKTOP, () => {
      try { win.webContents.send('images-updated', getDesktopImages()); } catch {}
    });
  } catch {}

  // Watch each project's .xmuggle/ dir
  for (const p of loadProjects()) {
    const xdir = path.join(p, '.xmuggle');
    try {
      fs.watch(xdir, { recursive: true }, () => {
        try { win.webContents.send('images-updated', getDesktopImages()); } catch {}
      });
    } catch {}
  }

  return win;
}

app.whenReady().then(() => {
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
  ipcMain.handle('add-project', async () => {
    const result = await dialog.showOpenDialog({ properties: ['openDirectory'] });
    if (result.canceled || !result.filePaths.length) return null;
    return addProject(result.filePaths[0]);
  });
  ipcMain.handle('remove-project', (_, dirPath) => {
    removeProject(dirPath);
    return listProjects();
  });

  // API key
  ipcMain.handle('has-api-key', () => api.hasApiKey());
  ipcMain.handle('set-api-key', (_, key) => { api.setApiKey(key); return true; });
  ipcMain.handle('reset-api-key', () => { api.resetApiKey(); return true; });
  ipcMain.handle('open-external', (_, url) => shell.openExternal(url));

  // Send
  ipcMain.handle('send-to-api', async (_, imagePaths, projectPath, message) => {
    const imgPath = imagePaths[0];
    const taskId = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);

    // Mark as processing
    updateTaskStatus(imgPath, projectPath, taskId, 'processing');

    try {
      const result = await api.analyzeAndFix({ imagePaths, projectPath, message });
      if (result.status === 'success') {
        updateTaskStatus(imgPath, projectPath, taskId, 'done', result.prUrl);
      } else {
        updateTaskStatus(imgPath, projectPath, taskId, result.status === 'no_changes' ? 'done' : 'error');
      }
      return result;
    } catch (err) {
      updateTaskStatus(imgPath, projectPath, taskId, 'error');
      throw err;
    }
  });

  createWindow();
});

app.on('window-all-closed', () => app.quit());
