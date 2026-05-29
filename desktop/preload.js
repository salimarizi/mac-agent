const { contextBridge, ipcRenderer } = require('electron');

contextBridge.exposeInMainWorld('api', {
  getConfig: () => ipcRenderer.invoke('get-config'),
  saveConfig: (cfg) => ipcRenderer.invoke('save-config', cfg),
  getStatus: () => ipcRenderer.invoke('get-status'),
  start: (cfg) => ipcRenderer.invoke('start', cfg),
  stop: () => ipcRenderer.invoke('stop'),
  checkBinary: () => ipcRenderer.invoke('check-binary'),
  buildBinary: () => ipcRenderer.invoke('build-binary'),

  onStatus: (cb) => {
    ipcRenderer.on('status', (_, s) => cb(s));
  },
  onLog: (cb) => {
    ipcRenderer.on('log', (_, msg) => cb(msg));
  },
});
