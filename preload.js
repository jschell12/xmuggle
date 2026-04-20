const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('xmuggle', {
  getImages: () => ipcRenderer.invoke('get-images'),
  deleteImage: (imgPath) => ipcRenderer.invoke('delete-image', imgPath),
  listProjects: () => ipcRenderer.invoke('list-projects'),
  addProject: () => ipcRenderer.invoke('add-project'),
  removeProject: (path) => ipcRenderer.invoke('remove-project', path),
  hasApiKey: () => ipcRenderer.invoke('has-api-key'),
  setApiKey: (key) => ipcRenderer.invoke('set-api-key', key),
  resetApiKey: () => ipcRenderer.invoke('reset-api-key'),
  hasGhToken: () => ipcRenderer.invoke('has-gh-token'),
  setGhToken: (token) => ipcRenderer.invoke('set-gh-token', token),
  resetGhToken: () => ipcRenderer.invoke('reset-gh-token'),
  sendToApi: (imagePaths, projectPath, message) => ipcRenderer.invoke('send-to-api', imagePaths, projectPath, message),
  sendFollowup: (imgPath, message) => ipcRenderer.invoke('send-followup', imgPath, message),
  getConversation: (imgPath) => ipcRenderer.invoke('get-conversation', imgPath),
  openExternal: (url) => ipcRenderer.invoke('open-external', url),
  onImagesUpdated: (callback) => {
    ipcRenderer.on('images-updated', (_, images) => callback(images));
  },
  onTaskProgress: (callback) => {
    ipcRenderer.on('task-progress', (_, imgPath, msg) => callback(imgPath, msg));
  },
});
