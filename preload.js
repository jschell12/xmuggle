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
  sendToApi: (imagePaths, projectPath, message) => ipcRenderer.invoke('send-to-api', imagePaths, projectPath, message),
  openExternal: (url) => ipcRenderer.invoke('open-external', url),
  onImagesUpdated: (callback) => {
    ipcRenderer.on('images-updated', (_, images) => callback(images));
  },
});
