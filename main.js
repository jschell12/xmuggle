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

function updateTaskStatus(imagePath, projectPath, taskId, status, prUrl, conversation, apiMessages) {
  const tasks = loadTasks();
  const existing = tasks[imagePath] || {};
  tasks[imagePath] = {
    projectPath,
    taskId: taskId || existing.taskId,
    status,
    prUrl: prUrl || existing.prUrl || '',
    conversation: conversation || existing.conversation || [],
    apiMessages: apiMessages || existing.apiMessages || [],
  };
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
        const conversation = task ? (task.conversation || []) : [];
        images.push({ path: full, name, mtime: stat.mtimeMs, status, projectPath, conversation });
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

  // GitHub token
  ipcMain.handle('has-gh-token', () => api.hasGhToken());
  ipcMain.handle('set-gh-token', (_, token) => { api.setGhToken(token); return true; });
  ipcMain.handle('reset-gh-token', () => { api.resetGhToken(); return true; });
  ipcMain.handle('open-external', (_, url) => shell.openExternal(url));

  // Send
  ipcMain.handle('send-to-api', async (event, imagePaths, projectPath, message) => {
    const imgPath = imagePaths[0];
    const taskId = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
    const win = BrowserWindow.fromWebContents(event.sender);

    // Mark as processing
    updateTaskStatus(imgPath, projectPath, taskId, 'processing');

    const onProgress = (msg) => {
      try { win.webContents.send('task-progress', imgPath, msg); } catch {}
    };

    try {
      const result = await api.analyzeAndFix({ imagePaths, projectPath, message, onProgress });
      const finalStatus = result.status === 'success' ? 'done' : (result.status === 'no_changes' ? 'done' : 'error');
      updateTaskStatus(imgPath, projectPath, taskId, finalStatus, result.prUrl, result.conversation, result.messages);
      return result;
    } catch (err) {
      updateTaskStatus(imgPath, projectPath, taskId, 'error');
      throw err;
    }
  });

  // Follow-up message on existing conversation
  ipcMain.handle('send-followup', async (event, imgPath, message) => {
    const tasks = loadTasks();
    const task = tasks[imgPath];
    if (!task) throw new Error('No task found for this image');

    const win = BrowserWindow.fromWebContents(event.sender);
    const onProgress = (msg) => {
      try { win.webContents.send('task-progress', imgPath, msg); } catch {}
    };

    updateTaskStatus(imgPath, task.projectPath, task.taskId, 'processing', task.prUrl, task.conversation, task.apiMessages);

    try {
      const result = await api.analyzeAndFix({
        imagePaths: [imgPath],
        projectPath: task.projectPath,
        message,
        onProgress,
        priorMessages: task.apiMessages,
      });
      const finalStatus = result.status === 'success' ? 'done' : (result.status === 'no_changes' ? 'done' : 'error');
      updateTaskStatus(imgPath, task.projectPath, task.taskId, finalStatus, result.prUrl || task.prUrl, result.conversation, result.messages);
      return result;
    } catch (err) {
      updateTaskStatus(imgPath, task.projectPath, task.taskId, 'error', task.prUrl, task.conversation, task.apiMessages);
      throw err;
    }
  });

  // Get conversation for an image
  ipcMain.handle('get-conversation', (_, imgPath) => {
    const tasks = loadTasks();
    const task = tasks[imgPath];
    return task ? (task.conversation || []) : [];
  });

  createWindow();
});

app.on('window-all-closed', () => app.quit());
