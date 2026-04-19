const grid = document.getElementById('grid');
const count = document.getElementById('count');
const apiKeySection = document.getElementById('api-key-section');
const apiKeyInput = document.getElementById('api-key-input');
const apiKeySave = document.getElementById('api-key-save');
const apiStatus = document.getElementById('api-status');
const toast = document.getElementById('toast');

const BADGE_LABELS = {
  new: 'New',
  pending: 'Pending',
  queued: 'Queued',
  processing: 'Processing',
  done: 'Done',
  error: 'Error',
};

const processingSet = new Set();

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

function render(images) {
  grid.innerHTML = '';

  const total = images.length;
  const pending = images.filter(i => i.status === 'new' || i.status === 'pending').length;
  const inProgress = images.filter(i => processingSet.has(i.path)).length;
  const done = images.filter(i => i.status === 'done').length;
  count.textContent = `${total} images \u2022 ${pending} pending \u2022 ${inProgress} in progress \u2022 ${done} done`;

  for (const img of images) {
    const isProcessing = processingSet.has(img.path);
    const card = document.createElement('div');
    card.className = 'card' + (isProcessing ? ' card-processing' : '');

    const imgEl = document.createElement('img');
    imgEl.src = `file://${img.path}`;
    imgEl.loading = 'lazy';
    card.appendChild(imgEl);

    const status = isProcessing ? 'processing' : img.status;
    const badge = document.createElement('span');
    badge.className = `badge ${status}`;
    badge.textContent = BADGE_LABELS[status] || status;
    card.appendChild(badge);

    // Send button
    if (!isProcessing && status !== 'done') {
      const sendBtn = document.createElement('button');
      sendBtn.className = 'send-btn';
      sendBtn.textContent = '\u25B6';
      sendBtn.title = 'Send to Claude';
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

function promptAndSend(img) {
  // Remove any existing modal
  const existing = document.getElementById('context-modal');
  if (existing) existing.remove();

  const modal = document.createElement('div');
  modal.id = 'context-modal';
  modal.className = 'modal-overlay';
  modal.innerHTML = `
    <div class="modal">
      <div class="modal-title">Send to Claude</div>
      <div class="modal-subtitle">${img.name}</div>
      <input type="text" id="repo-input" placeholder="owner/repo (e.g. jschell12/ai-enhance)" spellcheck="false">
      <textarea id="context-input" placeholder="What's wrong? What should be fixed?" rows="3"></textarea>
      <div class="modal-actions">
        <button id="modal-cancel" class="link-btn">Cancel</button>
        <button id="modal-send" class="modal-send-btn">Send</button>
      </div>
    </div>
  `;
  document.body.appendChild(modal);

  const repoInput = document.getElementById('repo-input');
  const contextInput = document.getElementById('context-input');
  repoInput.focus();

  document.getElementById('modal-cancel').addEventListener('click', () => modal.remove());
  modal.addEventListener('click', (e) => { if (e.target === modal) modal.remove(); });

  const doSend = () => {
    const repo = repoInput.value.trim();
    if (!repo) { repoInput.focus(); return; }
    const message = contextInput.value.trim();
    modal.remove();
    sendImage(img, repo, message);
  };

  document.getElementById('modal-send').addEventListener('click', doSend);
  contextInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) doSend();
  });
  repoInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') contextInput.focus();
  });
}

async function sendImage(img, repo, message) {
  processingSet.add(img.path);
  const images = await window.xmuggle.getImages();
  render(images);

  try {
    const result = await window.xmuggle.sendToApi([img.path], repo, message || '');
    processingSet.delete(img.path);

    if (result.status === 'success') {
      await window.xmuggle.deleteImage(img.path);
      const prInfo = result.prUrl ? ` PR: ${result.prUrl}` : '';
      showToast(`Fixed: ${result.summary}${prInfo}`, false);
    } else if (result.status === 'no_changes') {
      showToast(result.summary, false);
    } else {
      showToast(`Error: ${result.summary}`, true);
    }
  } catch (err) {
    processingSet.delete(img.path);
    showToast(`Error: ${err.message}`, true);
  }

  const updated = await window.xmuggle.getImages();
  render(updated);
}

async function initApiKey() {
  const hasKey = await window.xmuggle.hasApiKey();
  if (hasKey) {
    apiStatus.innerHTML = '';
    const label = document.createElement('span');
    label.textContent = 'API key set ';
    label.style.color = '#00b894';
    const resetBtn = document.createElement('button');
    resetBtn.className = 'link-btn';
    resetBtn.style.fontSize = '11px';
    resetBtn.textContent = 'Reset';
    resetBtn.addEventListener('click', async () => {
      await window.xmuggle.resetApiKey();
      initApiKey();
    });
    apiStatus.appendChild(label);
    apiStatus.appendChild(resetBtn);
    apiKeySection.style.display = 'none';
    apiStatus.style.display = '';
  } else {
    apiKeySection.style.display = 'flex';
    apiStatus.style.display = 'none';
  }
}

document.getElementById('api-key-get').addEventListener('click', () => {
  window.xmuggle.openExternal('https://console.anthropic.com/settings/keys');
});

apiKeySave.addEventListener('click', async () => {
  const key = apiKeyInput.value.trim();
  if (!key) return;
  await window.xmuggle.setApiKey(key);
  apiKeyInput.value = '';
  apiKeySection.style.display = 'none';
  apiStatus.style.display = '';
  apiStatus.textContent = 'API key set';
  apiStatus.style.color = '#00b894';
  showToast('API key saved', false);
});

apiKeyInput.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') apiKeySave.click();
});

async function refresh() {
  const images = await window.xmuggle.getImages();
  render(images);
}

window.xmuggle.onImagesUpdated((images) => render(images));
initApiKey();
refresh();
