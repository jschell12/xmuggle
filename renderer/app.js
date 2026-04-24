const grid = document.getElementById('grid');
const count = document.getElementById('count');
const relaySelect = document.getElementById('relay-select');
const relayStatusEl = document.getElementById('relay-status');
const toast = document.getElementById('toast');
const projectTabs = document.getElementById('project-tabs');
const addProjectBtn = document.getElementById('add-project');
const addNoteBtn = document.getElementById('add-note');
const settingsBtn = document.getElementById('settings-btn');

const daemonIndicator = document.getElementById('daemon-indicator');
const daemonDot = daemonIndicator.querySelector('.daemon-dot');
const daemonLabel = daemonIndicator.querySelector('.daemon-label');

const BADGE_LABELS = {
  new: 'New',
  pending: 'Pending',
  queued: 'Queued',
  processing: 'Processing',
  done: 'Done',
  error: 'Error',
};

let projects = [];
let activeProject = null; // path of selected project, or null for "all"
const processingSet = new Set();
const progressLogs = {}; // imgPath -> [messages]

// ── Daemon ──

let daemonRunning = false;

async function updateDaemonStatus() {
  try {
    const status = await window.xmuggle.daemonStatus();
    daemonRunning = status.running;
    daemonDot.className = 'daemon-dot ' + (status.running ? 'daemon-on' : 'daemon-off');
    daemonLabel.textContent = status.running ? `Daemon (pid ${status.pid})` : 'Daemon (stopped)';
    daemonIndicator.title = status.running ? `Daemon running (pid ${status.pid})` : 'Daemon stopped — click to start';
  } catch {
    daemonDot.className = 'daemon-dot daemon-off';
    daemonLabel.textContent = 'Daemon (unknown)';
  }
}

daemonIndicator.addEventListener('click', async () => {
  if (daemonRunning) {
    const result = await window.xmuggle.daemonStop();
    showToast(result.ok ? 'Daemon stopped' : `Stop failed: ${result.message}`, !result.ok);
  } else {
    const result = await window.xmuggle.daemonStart();
    showToast(result.ok ? 'Daemon started' : `Start failed: ${result.message}`, !result.ok);
  }
  await updateDaemonStatus();
});

// ── Toast ──

function showToast(msg, isError) {
  toast.innerHTML = '';
  const text = document.createElement('span');
  text.textContent = msg;
  const closeBtn = document.createElement('button');
  closeBtn.className = 'toast-close';
  closeBtn.textContent = '\u00d7';
  closeBtn.addEventListener('click', () => toast.className = 'toast hidden');
  toast.appendChild(text);
  toast.appendChild(closeBtn);
  toast.className = `toast ${isError ? 'toast-error' : 'toast-success'}`;
}

// ── URL Detection ──

function makeLinksClickable(text) {
  const urlRegex = /(https?:\/\/[^\s]+)/g;
  const parts = text.split(urlRegex);

  const container = document.createElement('span');

  parts.forEach(part => {
    if (/^https?:\/\/[^\s]+$/.test(part)) {
      const link = document.createElement('a');
      link.href = part;
      link.textContent = part;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      link.style.color = '#74b9ff';
      link.style.textDecoration = 'underline';
      link.addEventListener('click', (e) => {
        e.preventDefault();
        e.stopPropagation();
        window.xmuggle.openExternal(part);
      });
      container.appendChild(link);
    } else {
      container.appendChild(document.createTextNode(part));
    }
  });

  return container;
}

// ── Projects ──

async function loadProjects() {
  projects = await window.xmuggle.listProjects();
  renderProjectTabs();
}

function renderProjectTabs() {
  projectTabs.innerHTML = '';

  // "All" tab
  const allTab = document.createElement('div');
  allTab.className = 'project-tab' + (activeProject === null ? ' project-tab-active' : '');
  allTab.textContent = 'All';
  allTab.addEventListener('click', () => {
    activeProject = null;
    renderProjectTabs();
    refresh();
  });
  projectTabs.appendChild(allTab);

  for (const p of projects) {
    const tab = document.createElement('div');
    tab.className = 'project-tab' + (activeProject === p.path ? ' project-tab-active' : '');
    tab.title = p.path;

    const nameSpan = document.createElement('span');
    nameSpan.textContent = p.name;
    tab.appendChild(nameSpan);

    tab.addEventListener('click', () => {
      activeProject = activeProject === p.path ? null : p.path;
      renderProjectTabs();
      refresh();
    });

    const removeBtn = document.createElement('button');
    removeBtn.className = 'project-remove';
    removeBtn.textContent = '\u00d7';
    removeBtn.addEventListener('click', async (e) => {
      e.stopPropagation();
      await window.xmuggle.removeProject(p.path);
      if (activeProject === p.path) activeProject = null;
      await loadProjects();
      refresh();
    });
    tab.appendChild(removeBtn);
    projectTabs.appendChild(tab);
  }
}

addProjectBtn.addEventListener('click', () => {
  const existing = document.getElementById('add-project-modal');
  if (existing) existing.remove();

  const modal = document.createElement('div');
  modal.id = 'add-project-modal';
  modal.className = 'modal-overlay';
  modal.innerHTML = `
    <div class="modal">
      <div class="modal-title">Add Project</div>
      <div class="settings-field">
        <label>Git URL</label>
        <input type="text" id="add-project-git-url" placeholder="git@github.com:user/repo.git (auto-detected if blank)">
        <div class="settings-hint">Leave blank to auto-detect from the selected directory</div>
      </div>
      <div class="settings-field">
        <label>Local Path</label>
        <div style="display:flex;gap:8px;align-items:center;">
          <input type="text" id="add-project-path" placeholder="Select a directory..." readonly style="flex:1;cursor:pointer;">
          <button id="add-project-browse" class="link-btn">Browse</button>
        </div>
      </div>
      <div class="modal-actions">
        <button id="add-project-cancel" class="link-btn">Cancel</button>
        <button id="add-project-save" class="modal-send-btn">Add</button>
      </div>
    </div>
  `;
  document.body.appendChild(modal);

  modal.addEventListener('click', (e) => { if (e.target === modal) modal.remove(); });
  document.getElementById('add-project-cancel').addEventListener('click', () => modal.remove());

  document.getElementById('add-project-browse').addEventListener('click', async () => {
    // Use the old addProject() with no args to trigger directory picker
    const result = await window.xmuggle.addProject();
    if (result && !result.error) {
      document.getElementById('add-project-path').value = result.path;
      if (result.gitUrl) {
        document.getElementById('add-project-git-url').value = result.gitUrl;
      }
      // Already added via the picker, close modal
      modal.remove();
      await loadProjects();
      showToast(`Added project: ${result.name}`, false);
    } else if (result && result.error) {
      showToast(result.error, true);
    }
  });

  document.getElementById('add-project-save').addEventListener('click', async () => {
    const gitUrl = document.getElementById('add-project-git-url').value.trim();
    const dirPath = document.getElementById('add-project-path').value.trim();
    if (!dirPath) {
      // Trigger directory picker if no path entered
      document.getElementById('add-project-browse').click();
      return;
    }
    modal.remove();
    const result = await window.xmuggle.addProject(gitUrl, dirPath);
    if (result && !result.error) {
      await loadProjects();
      showToast(`Added project: ${result.name}`, false);
    } else if (result && result.error) {
      showToast(result.error, true);
    }
  });
});

// ── Paste text note ──

addNoteBtn.addEventListener('click', () => {
  const existing = document.getElementById('note-modal');
  if (existing) existing.remove();

  let projectOptions = '';
  for (const p of projects) {
    const selected = (activeProject === p.path) ? ' selected' : '';
    projectOptions += `<option value="${p.path}"${selected}>${p.name}</option>`;
  }
  if (projects.length === 0) {
    projectOptions = '<option value="">No projects \u2014 add one first</option>';
  }

  const modal = document.createElement('div');
  modal.id = 'note-modal';
  modal.className = 'modal-overlay';
  modal.innerHTML = `
    <div class="modal">
      <div class="modal-title">Paste text</div>
      <div class="modal-subtitle">Saved as a text note you can send like a screenshot</div>
      <label class="modal-label">Project</label>
      <select id="note-project-select">${projectOptions}</select>
      <textarea id="note-text-input" placeholder="Paste error message, stack trace, log, etc\u2026" rows="10"></textarea>
      <div class="modal-actions">
        <button id="note-cancel" class="link-btn">Cancel</button>
        <button id="note-save" class="modal-send-btn">Save</button>
      </div>
    </div>
  `;
  document.body.appendChild(modal);

  const textInput = document.getElementById('note-text-input');
  textInput.focus();
  modal.addEventListener('click', (e) => { if (e.target === modal) modal.remove(); });
  document.getElementById('note-cancel').addEventListener('click', () => modal.remove());

  const save = async () => {
    const text = textInput.value.trim();
    if (!text) return;
    const projectPath = document.getElementById('note-project-select').value;
    if (!projectPath) { showToast('Select a project first', true); return; }
    modal.remove();
    try {
      const note = await window.xmuggle.createNote(text);
      // Push to queue just like screenshots
      const result = await window.xmuggle.queuePush([note.path], projectPath, '');
      showToast('Queued ' + note.name, false);
      await refresh();
    } catch (err) {
      showToast(`Error: ${err.message}`, true);
    }
  };
  document.getElementById('note-save').addEventListener('click', save);
  textInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) save();
  });
});

// ── Settings Modal ──

settingsBtn.addEventListener('click', async () => {
  const existing = document.getElementById('settings-modal');
  if (existing) { existing.remove(); return; }

  const queueUrl = await window.xmuggle.getQueueUrl();
  const hasToken = await window.xmuggle.hasGhToken();
  const daemon = await window.xmuggle.daemonStatus();
  const daemonCfg = await window.xmuggle.getDaemonConfig();
  const repos = (daemonCfg.repos || []).filter(r => r.path);

  const modal = document.createElement('div');
  modal.id = 'settings-modal';
  modal.className = 'modal-overlay';
  modal.innerHTML = `
    <div class="modal">
      <div class="modal-title">Settings</div>
      <div class="settings-field">
        <label>Daemon</label>
        <div class="daemon-control">
          <span class="daemon-dot ${daemon.running ? 'daemon-on' : 'daemon-off'}"></span>
          <span id="settings-daemon-status">${daemon.running ? 'Running (pid ' + daemon.pid + ')' : 'Stopped'}</span>
          <button id="settings-daemon-toggle" class="daemon-toggle-btn ${daemon.running ? 'daemon-stop-btn' : 'daemon-start-btn'}">${daemon.running ? 'Stop' : 'Start'}</button>
          <button id="settings-daemon-log" class="link-btn">View Log</button>
        </div>
        <div class="settings-hint">Background daemon syncs repos and processes queue tasks</div>
      </div>
      <div class="settings-field">
        <label>Queue Repo URL</label>
        <input type="text" id="settings-queue-url" placeholder="git@github.com:user/xmuggle-queue.git">
        <div class="settings-hint">Git repo for syncing screenshots between machines</div>
      </div>
      <div class="settings-field">
        <label>GitHub Token ${hasToken ? '(set)' : '(not set)'}</label>
        <input type="password" id="settings-gh-token" value="" placeholder="${hasToken ? 'Leave blank to keep current' : 'ghp_...'}">
        <div class="settings-hint">Used for git push auth. Leave blank to keep current value.</div>
      </div>
      <div class="settings-field">
        <label>Repos &amp; Post Commands</label>
        <div id="settings-repos"></div>
        <div class="settings-hint">After a task completes, pull changes and run these commands in the local repo. Add projects above to see them here.</div>
      </div>
      <div class="modal-actions">
        <button id="settings-cancel" class="link-btn">Cancel</button>
        ${hasToken ? '<button id="settings-reset-token" class="link-btn" style="color:#d63031;">Reset Token</button>' : ''}
        <button id="settings-save" class="modal-send-btn">Save</button>
      </div>
    </div>
  `;
  document.body.appendChild(modal);
  document.getElementById('settings-queue-url').value = queueUrl || '';

  // Render repos list — one row per configured project
  const reposContainer = document.getElementById('settings-repos');
  // Ensure every project has a repo entry
  for (const p of projects) {
    if (!repos.find(r => r.path === p.path)) {
      repos.push({ path: p.path, postCommands: [] });
    }
  }
  function renderRepos() {
    reposContainer.innerHTML = '';
    repos.forEach((repo, i) => {
      const name = repo.path ? repo.path.split('/').pop() : '(unknown)';
      const cmds = (repo.postCommands || []).join('; ');
      const row = document.createElement('div');
      row.className = 'settings-repo-row';
      row.innerHTML = `
        <span class="repo-name">${name}</span>
        <input type="text" class="repo-cmds" value="${cmds}" placeholder="make build; make install">
      `;
      row.querySelector('.repo-cmds').addEventListener('change', (e) => {
        const val = e.target.value.trim();
        repos[i].postCommands = val ? val.split(';').map(s => s.trim()).filter(Boolean) : [];
      });
      reposContainer.appendChild(row);
    });
  }
  renderRepos();

  modal.addEventListener('click', (e) => { if (e.target === modal) modal.remove(); });
  document.getElementById('settings-cancel').addEventListener('click', () => modal.remove());

  // Daemon toggle
  document.getElementById('settings-daemon-toggle').addEventListener('click', async (e) => {
    const btn = e.target;
    btn.disabled = true;
    btn.textContent = '...';
    const result = daemon.running
      ? await window.xmuggle.daemonStop()
      : await window.xmuggle.daemonStart();
    const updated = await window.xmuggle.daemonStatus();
    const dot = modal.querySelector('.daemon-dot');
    dot.className = 'daemon-dot ' + (updated.running ? 'daemon-on' : 'daemon-off');
    document.getElementById('settings-daemon-status').textContent =
      updated.running ? 'Running (pid ' + updated.pid + ')' : 'Stopped';
    btn.textContent = updated.running ? 'Stop' : 'Start';
    btn.className = 'daemon-toggle-btn ' + (updated.running ? 'daemon-stop-btn' : 'daemon-start-btn');
    btn.disabled = false;
    daemon.running = updated.running;
    daemon.pid = updated.pid;
    await updateDaemonStatus();
    showToast(result.ok ? result.message : `Error: ${result.message}`, !result.ok);
  });

  // Daemon log viewer
  document.getElementById('settings-daemon-log').addEventListener('click', async () => {
    const existing = document.getElementById('daemon-log-modal');
    if (existing) { existing.remove(); return; }
    const logText = await window.xmuggle.daemonLog(80);
    const logModal = document.createElement('div');
    logModal.id = 'daemon-log-modal';
    logModal.className = 'modal-overlay';
    logModal.innerHTML = `
      <div class="modal" style="max-width:700px;max-height:80vh;overflow-y:auto;">
        <div class="modal-title">Daemon Log</div>
        <pre class="daemon-log-content">${logText || 'No log output'}</pre>
        <div class="modal-actions">
          <button id="daemon-log-refresh" class="link-btn">Refresh</button>
          <button id="daemon-log-close" class="modal-send-btn">Close</button>
        </div>
      </div>
    `;
    document.body.appendChild(logModal);
    logModal.addEventListener('click', (e) => { if (e.target === logModal) logModal.remove(); });
    document.getElementById('daemon-log-close').addEventListener('click', () => logModal.remove());
    document.getElementById('daemon-log-refresh').addEventListener('click', async () => {
      const fresh = await window.xmuggle.daemonLog(80);
      logModal.querySelector('.daemon-log-content').textContent = fresh || 'No log output';
    });
  });

  const resetBtn = document.getElementById('settings-reset-token');
  if (resetBtn) {
    resetBtn.addEventListener('click', async () => {
      await window.xmuggle.resetGhToken();
      modal.remove();
      showToast('GitHub token cleared', false);
    });
  }

  document.getElementById('settings-save').addEventListener('click', async () => {
    const newQueue = document.getElementById('settings-queue-url').value.trim();
    const newToken = document.getElementById('settings-gh-token').value.trim();

    if (newQueue !== (queueUrl || '')) {
      await window.xmuggle.setQueueUrl(newQueue);
    }
    if (newToken) {
      await window.xmuggle.setGhToken(newToken);
    }

    // Save repos config
    const validRepos = repos.filter(r => r.path);
    daemonCfg.repos = validRepos;
    await window.xmuggle.setDaemonConfig(daemonCfg);

    modal.remove();
    showToast('Settings saved', false);
  });
});

// ── Result Modal ──

function showResultModal(img) {
  const existing = document.getElementById('result-modal');
  if (existing) existing.remove();

  const modal = document.createElement('div');
  modal.id = 'result-modal';
  modal.className = 'modal-overlay';
  modal.innerHTML = `
    <div class="modal" style="max-width:600px;max-height:80vh;overflow-y:auto;">
      <div class="modal-title">Result</div>
      <div class="modal-subtitle">${img.name} \u2192 ${img.projectPath ? img.projectPath.split('/').pop() : ''}</div>
      <pre class="result-text">${img.result || 'No result'}</pre>
      <div class="modal-actions">
        <button id="result-close" class="modal-send-btn">Close</button>
      </div>
    </div>
  `;
  document.body.appendChild(modal);
  modal.addEventListener('click', (e) => { if (e.target === modal) modal.remove(); });
  document.getElementById('result-close').addEventListener('click', () => modal.remove());
}

// ── Images ──

function render(images) {
  grid.innerHTML = '';

  // Filter by active project
  const filtered = activeProject
    ? images.filter(i => i.projectPath === activeProject || (!i.projectPath && i.status === 'new'))
    : images;

  const total = filtered.length;
  const pending = filtered.filter(i => i.status === 'new').length;
  const inProgress = filtered.filter(i => i.status === 'processing' || processingSet.has(i.path)).length;
  const done = filtered.filter(i => i.status === 'done').length;
  const label = activeProject ? activeProject.split('/').pop() : 'all projects';
  count.textContent = `${total} items \u2022 ${pending} new \u2022 ${inProgress} in progress \u2022 ${done} done \u2022 ${label}`;

  for (const img of filtered) {
    const isProcessing = processingSet.has(img.path);
    const isText = img.type === 'text';
    const card = document.createElement('div');
    card.className = 'card'
      + (isText ? ' card-text' : '')
      + (isProcessing ? ' card-processing' : '');

    if (isText) {
      const textEl = document.createElement('div');
      textEl.className = 'text-preview';
      textEl.textContent = img.preview || '';
      card.appendChild(textEl);
    } else {
      const imgEl = document.createElement('img');
      imgEl.src = `file://${img.path}`;
      imgEl.loading = 'lazy';
      card.appendChild(imgEl);
    }

    const status = isProcessing ? 'processing' : img.status;
    const badge = document.createElement('span');
    badge.className = `badge ${status}`;
    badge.textContent = BADGE_LABELS[status] || status;
    card.appendChild(badge);

    // Project label if assigned
    if (img.projectPath) {
      const projLabel = document.createElement('div');
      projLabel.className = 'project-label';
      projLabel.textContent = img.projectPath.split('/').pop();
      card.appendChild(projLabel);
    }

    // Progress log (during processing)
    if (isProcessing && progressLogs[img.path] && progressLogs[img.path].length > 0) {
      const logEl = document.createElement('div');
      logEl.className = 'progress-log';
      logEl.id = `log-${CSS.escape(img.path)}`;
      for (const msg of progressLogs[img.path]) {
        const line = document.createElement('div');
        line.className = 'progress-line';
        line.textContent = msg;
        logEl.appendChild(line);
      }
      card.appendChild(logEl);
      requestAnimationFrame(() => { logEl.scrollTop = logEl.scrollHeight; });
    }

    // Result summary (when done)
    if (status === 'done' && img.result) {
      const resultEl = document.createElement('div');
      resultEl.className = 'result-summary';
      resultEl.textContent = img.result.length > 200
        ? img.result.slice(0, 200) + '\u2026'
        : img.result;
      resultEl.title = 'Click to see full result';
      resultEl.addEventListener('click', (e) => {
        e.stopPropagation();
        showResultModal(img);
      });
      card.appendChild(resultEl);
    }

    // Send button
    if (!isProcessing && status !== 'done') {
      const sendBtn = document.createElement('button');
      sendBtn.className = 'send-btn';
      sendBtn.textContent = '\u25B6';
      sendBtn.title = 'Send';
      sendBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        promptAndSend(img);
      });
      card.appendChild(sendBtn);
    }

    // Delete button
    const deleteBtn = document.createElement('button');
    deleteBtn.className = 'delete-btn';
    deleteBtn.textContent = '\u00d7';
    deleteBtn.title = 'Delete screenshot';
    deleteBtn.addEventListener('click', async (e) => {
      e.stopPropagation();
      const images = await window.xmuggle.deleteImage(img.path);
      render(images);
    });
    card.appendChild(deleteBtn);

    const name = document.createElement('div');
    name.className = 'name';
    name.textContent = img.name;
    name.title = img.name;
    card.appendChild(name);

    grid.appendChild(card);
  }
}

// ── Send Modal ──

function promptAndSend(img) {
  const existing = document.getElementById('context-modal');
  if (existing) existing.remove();

  let projectOptions = '';
  for (const p of projects) {
    const selected = (activeProject === p.path) ? ' selected' : '';
    projectOptions += `<option value="${p.path}"${selected}>${p.name}</option>`;
  }
  if (projects.length === 0) {
    projectOptions = '<option value="">No projects \u2014 add one first</option>';
  }

  const modal = document.createElement('div');
  modal.id = 'context-modal';
  modal.className = 'modal-overlay';
  modal.innerHTML = `
    <div class="modal">
      <div class="modal-title">Send</div>
      <div class="modal-subtitle">${img.name}</div>
      <label class="modal-label">Project</label>
      <select id="project-select">${projectOptions}</select>
      <label class="modal-label">Context</label>
      <textarea id="context-input" placeholder="What's wrong? What should be fixed?" rows="3"></textarea>
      <div class="modal-actions">
        <button id="modal-cancel" class="link-btn">Cancel</button>
        <button id="modal-send" class="modal-send-btn" ${projects.length === 0 ? 'disabled' : ''}>Send</button>
      </div>
    </div>
  `;
  document.body.appendChild(modal);

  const contextInput = document.getElementById('context-input');
  const projectSelect = document.getElementById('project-select');
  projectSelect.focus();

  document.getElementById('modal-cancel').addEventListener('click', () => modal.remove());
  modal.addEventListener('click', (e) => { if (e.target === modal) modal.remove(); });

  const doSend = () => {
    const projectPath = projectSelect.value;
    if (!projectPath) return;
    const message = contextInput.value.trim();
    modal.remove();

    queuePush(img, projectPath, message);
  };

  document.getElementById('modal-send').addEventListener('click', doSend);
  contextInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) doSend();
  });
}

async function saveItem(img, projectPath, message) {
  try {
    await window.xmuggle.saveItem(img.path, projectPath, message || '');
    showToast('Saved', false);
  } catch (err) {
    showToast(`Error: ${err.message}`, true);
  }
  const updated = await window.xmuggle.getImages();
  render(updated);
}

async function queuePush(img, projectPath, message) {
  // Ensure queue URL is configured
  const queueUrl = await window.xmuggle.getQueueUrl();
  if (!queueUrl) {
    showToast('Set queue repo URL in Settings first', true);
    return;
  }

  processingSet.add(img.path);
  const images = await window.xmuggle.getImages();
  render(images);

  try {
    const result = await window.xmuggle.queuePush([img.path], projectPath, message || '');
    processingSet.delete(img.path);
    showToast('Queued for ' + (result.project || projectPath.split('/').pop()), false);
  } catch (err) {
    processingSet.delete(img.path);
    showToast('Queue error: ' + err.message, true);
  }

  const updated = await window.xmuggle.getImages();
  render(updated);
}

async function relayImage(img, projectPath, message) {
  processingSet.add(img.path);
  const images = await window.xmuggle.getImages();
  render(images);

  try {
    const result = await window.xmuggle.sendToRelay(img.path, projectPath, message || '');
    processingSet.delete(img.path);
    showToast('Sent to relay: ' + (result.path || 'received'), false);
  } catch (err) {
    processingSet.delete(img.path);
    showToast('Relay error: ' + err.message, true);
  }

  const updated = await window.xmuggle.getImages();
  render(updated);
}

// ── Relay Network ──

let discoveredHosts = [];

async function initRelay() {
  const saved = await window.xmuggle.getRelayHost();

  relaySelect.innerHTML = '<option value="">Local (no relay)</option><option value="_scan">Scanning network...</option>';
  if (saved) relaySelect.value = saved;

  try {
    const hosts = await window.xmuggle.scanNetwork();
    discoveredHosts = hosts;
    relaySelect.innerHTML = '<option value="">Local (no relay)</option>';
    for (const h of hosts) {
      const opt = document.createElement('option');
      opt.value = h.ip;
      opt.textContent = `${h.hostname} (${h.ip})`;
      if (h.ip === saved) opt.selected = true;
      relaySelect.appendChild(opt);
    }

    // Git sync option
    const gitOpt = document.createElement('option');
    gitOpt.value = '_git';
    gitOpt.textContent = 'Git sync';
    relaySelect.appendChild(gitOpt);

    if (hosts.length === 0) {
      // Default to git sync when no local peers found
      relaySelect.value = '_git';
      await window.xmuggle.setRelayHost('_git');
      relayStatusEl.textContent = 'No peers \u2014 using git sync';
      relayStatusEl.style.color = '#f0a500';
    } else {
      relayStatusEl.textContent = hosts.length + ' peer(s)';
      relayStatusEl.style.color = '#00b894';
    }

    // Add rescan option
    const rescan = document.createElement('option');
    rescan.value = '_scan';
    rescan.textContent = 'Rescan...';
    relaySelect.appendChild(rescan);
  } catch {
    relaySelect.innerHTML = '<option value="">Local (no relay)</option>';
    relayStatusEl.textContent = 'Scan failed';
    relayStatusEl.style.color = '#d63031';
  }
}

relaySelect.addEventListener('change', async () => {
  const val = relaySelect.value;
  if (val === '_scan') {
    relayStatusEl.textContent = 'Scanning...';
    relayStatusEl.style.color = '#f0a500';
    await initRelay();
    return;
  }
  if (val === '_git') {
    await window.xmuggle.setRelayHost('_git');
    showToast('Using git sync', false);
    return;
  }
  await window.xmuggle.setRelayHost(val);
  if (val) {
    showToast('Relay: ' + val, false);
  } else {
    showToast('Relay disabled (local mode)', false);
  }
});

// ── Init ──

async function refresh() {
  const images = await window.xmuggle.getImages();
  render(images);
}

window.xmuggle.onImagesUpdated((images) => render(images));
window.xmuggle.onTaskProgress((imgPath, msg) => {
  if (!progressLogs[imgPath]) progressLogs[imgPath] = [];
  progressLogs[imgPath].push(msg);
  const logEl = document.getElementById(`log-${CSS.escape(imgPath)}`);
  if (logEl) {
    const line = document.createElement('div');
    line.className = 'progress-line';
    line.textContent = msg;
    logEl.appendChild(line);
    logEl.scrollTop = logEl.scrollHeight;
  } else {
    refresh();
  }
});
initRelay();
loadProjects();
refresh();
updateDaemonStatus();
setInterval(updateDaemonStatus, 10_000);
