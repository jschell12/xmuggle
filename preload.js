const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('xmuggle', {
  getImages: () => ipcRenderer.invoke('get-images'),
  deleteImage: (imgPath) => ipcRenderer.invoke('delete-image', imgPath),
  listProjects: () => ipcRenderer.invoke('list-projects'),
  addProject: (gitUrl, dirPath) => ipcRenderer.invoke('add-project', gitUrl, dirPath),
  removeProject: (path) => ipcRenderer.invoke('remove-project', path),
  hasGhToken: () => ipcRenderer.invoke('has-gh-token'),
  setGhToken: (token) => ipcRenderer.invoke('set-gh-token', token),
  resetGhToken: () => ipcRenderer.invoke('reset-gh-token'),
  saveItem: (imagePath, projectPath, message) => ipcRenderer.invoke('save-item', imagePath, projectPath, message),
  createNote: (text) => ipcRenderer.invoke('create-note', text),
  getRelayHost: () => ipcRenderer.invoke('get-relay-host'),
  setRelayHost: (host) => ipcRenderer.invoke('set-relay-host', host),
  scanNetwork: () => ipcRenderer.invoke('scan-network'),
  sendToRelay: (imagePath, project, message) => ipcRenderer.invoke('send-to-relay', imagePath, project, message),
  getQueueUrl: () => ipcRenderer.invoke('get-queue-url'),
  setQueueUrl: (url) => ipcRenderer.invoke('set-queue-url', url),
  queuePush: (imagePaths, projectPath, message) => ipcRenderer.invoke('queue-push', imagePaths, projectPath, message),
  openExternal: (url) => ipcRenderer.invoke('open-external', url),
  daemonStatus: () => ipcRenderer.invoke('daemon-status'),
  daemonStart: () => ipcRenderer.invoke('daemon-start'),
  daemonStop: () => ipcRenderer.invoke('daemon-stop'),
  daemonLog: (lines) => ipcRenderer.invoke('daemon-log', lines),
  getDaemonConfig: () => ipcRenderer.invoke('get-daemon-config'),
  setDaemonConfig: (cfg) => ipcRenderer.invoke('set-daemon-config', cfg),
  onImagesUpdated: (callback) => {
    ipcRenderer.on('images-updated', (_, images) => callback(images));
  },
  onTaskProgress: (callback) => {
    ipcRenderer.on('task-progress', (_, imgPath, msg) => callback(imgPath, msg));
  },
});
