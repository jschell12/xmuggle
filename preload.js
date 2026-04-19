const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('xmuggle', {
  getImages: () => ipcRenderer.invoke('get-images'),
  deleteImage: (imgPath) => ipcRenderer.invoke('delete-image', imgPath),
  hasApiKey: () => ipcRenderer.invoke('has-api-key'),
  setApiKey: (key) => ipcRenderer.invoke('set-api-key', key),
  resetApiKey: () => ipcRenderer.invoke('reset-api-key'),
  sendToApi: (imagePaths, message) => ipcRenderer.invoke('send-to-api', imagePaths, message),
  openExternal: (url) => ipcRenderer.invoke('open-external', url),
  onImagesUpdated: (callback) => {
    ipcRenderer.on('images-updated', (_, images) => callback(images));
  },
});
