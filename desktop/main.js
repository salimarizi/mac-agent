const { app, BrowserWindow, ipcMain, Tray, Menu, nativeImage } = require('electron');
const path = require('path');
const fs = require('fs');
const { spawn, exec } = require('child_process');

// ── Paths ────────────────────────────────────────────────────────────────────

const configPath = path.join(app.getPath('userData'), 'config.json');
const isDev = !app.isPackaged;
const binPath = isDev
  ? path.join(__dirname, 'bin', 'mac-agent')
  : path.join(process.resourcesPath, 'bin', 'mac-agent');

// ── State ────────────────────────────────────────────────────────────────────

let mainWindow = null;
let tray = null;
let agentProcess = null;
let sshProcess = null;
let isRunning = false;

// ── Config ───────────────────────────────────────────────────────────────────

function loadEnv() {
  const envPaths = [
    path.join(process.cwd(), '.env'),
    path.join(__dirname, '..', '.env'),
    path.join(__dirname, '.env'),
  ];

  for (const envPath of envPaths) {
    if (fs.existsSync(envPath)) {
      try {
        const content = fs.readFileSync(envPath, 'utf8');
        const lines = content.split(/\r?\n/);
        for (let line of lines) {
          line = line.trim();
          if (!line || line.startsWith('#')) continue;

          const firstEqual = line.indexOf('=');
          if (firstEqual === -1) continue;

          const key = line.substring(0, firstEqual).trim();
          let value = line.substring(firstEqual + 1).trim();

          // Remove comments at the end of the line, unless they are inside quotes
          if (value.includes('#')) {
            const hasDoubleQuotes = value.startsWith('"') && value.endsWith('"');
            const hasSingleQuotes = value.startsWith("'") && value.endsWith("'");
            if (!hasDoubleQuotes && !hasSingleQuotes) {
              const hashIdx = value.indexOf('#');
              value = value.substring(0, hashIdx).trim();
            }
          }

          // Strip surrounding quotes
          if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
            value = value.substring(1, value.length - 1);
          }

          if (key && process.env[key] === undefined) {
            process.env[key] = value;
          }
        }
        break;
      } catch (err) {
        console.error(`[Env] Failed to load .env from ${envPath}:`, err.message);
      }
    }
  }
}

// Load env file variables
loadEnv();

const defaultConfig = {
  vpsHost: process.env.MAC_AGENT_VPS_HOST || process.env.VPS_HOST,
  vpsUser: process.env.MAC_AGENT_VPS_USER || process.env.VPS_USER,
  vpsPort: process.env.MAC_AGENT_VPS_PORT || process.env.VPS_PORT,
  tunnelPort: process.env.MAC_AGENT_TUNNEL_PORT || process.env.TUNNEL_PORT,
  password: process.env.MAC_AGENT_PASSWORD || process.env.AGENT_PASSWORD,
  turnUrl: process.env.MAC_AGENT_TURN_URL || process.env.TURN_URL,
  turnUser: process.env.MAC_AGENT_TURN_USER || process.env.TURN_USER,
  turnPass: process.env.MAC_AGENT_TURN_PASS || process.env.TURN_PASS,
  screenIndex: process.env.MAC_AGENT_SCREEN_INDEX || process.env.SCREEN_INDEX || '0',
  fps: process.env.MAC_AGENT_FPS || process.env.FPS || '30',
  width: process.env.MAC_AGENT_WIDTH || process.env.WIDTH || '1600',
  bitrate: process.env.MAC_AGENT_BITRATE || process.env.BITRATE || '6M',
};

function loadConfig() {
  let savedConfig = {};
  try {
    if (fs.existsSync(configPath)) {
      savedConfig = JSON.parse(fs.readFileSync(configPath, 'utf8'));
    }
  } catch {}

  const config = { ...defaultConfig, ...savedConfig };

  // Environment variables override saved settings if present
  const envMapping = {
    vpsHost: ['MAC_AGENT_VPS_HOST', 'VPS_HOST'],
    vpsUser: ['MAC_AGENT_VPS_USER', 'VPS_USER'],
    vpsPort: ['MAC_AGENT_VPS_PORT', 'VPS_PORT'],
    tunnelPort: ['MAC_AGENT_TUNNEL_PORT', 'TUNNEL_PORT'],
    password: ['MAC_AGENT_PASSWORD', 'AGENT_PASSWORD'],
    turnUrl: ['MAC_AGENT_TURN_URL', 'TURN_URL'],
    turnUser: ['MAC_AGENT_TURN_USER', 'TURN_USER'],
    turnPass: ['MAC_AGENT_TURN_PASS', 'TURN_PASS'],
    screenIndex: ['MAC_AGENT_SCREEN_INDEX', 'SCREEN_INDEX'],
    fps: ['MAC_AGENT_FPS', 'FPS'],
    width: ['MAC_AGENT_WIDTH', 'WIDTH'],
    bitrate: ['MAC_AGENT_BITRATE', 'BITRATE']
  };

  for (const [key, envKeys] of Object.entries(envMapping)) {
    for (const envKey of envKeys) {
      if (process.env[envKey] !== undefined && process.env[envKey] !== '') {
        config[key] = process.env[envKey];
        break;
      }
    }
  }

  return config;
}

function saveConfig(cfg) {
  fs.writeFileSync(configPath, JSON.stringify(cfg, null, 2), { mode: 0o600 });
}

// ── Window ───────────────────────────────────────────────────────────────────

function createWindow() {
  mainWindow = new BrowserWindow({
    width: 480,
    height: 720,
    minWidth: 420,
    minHeight: 600,
    titleBarStyle: 'hiddenInset',
    backgroundColor: '#1a1a2e',
    resizable: true,
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
    },
  });

  mainWindow.loadFile('index.html');

  mainWindow.on('close', (e) => {
    if (isRunning) {
      e.preventDefault();
      mainWindow.hide();
    }
  });
}

// ── Tray ─────────────────────────────────────────────────────────────────────

function createTray() {
  const icon = nativeImage.createFromDataURL(
    'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAABAAAAAQCAYAAAAf8/9hAAAAmklEQVQ4T2NkoBAwUqifYdAb8P9/' +
    'gwMDA8N+BgYGByL9vJ+BgeEAIwMDgwMxBjAyMDgwMjLuZ/j/34GRkfEAMYYcYGBk3M/AwODAyMi4n+H/fwdiDGH4/9+BgYHhACMj' +
    '434GBgYHYgw5wMDA4MDIwHAAq8v+/3dgYGDYz8DA4MDAwLCfGJcxMjDsZ2Bg2M/AwOBAjCEUAwBfLjQRaUDVhgAAAABJRU5ErkJggg=='
  );
  tray = new Tray(icon);
  tray.setToolTip('Mac Remote');
  updateTrayMenu();
}

function updateTrayMenu() {
  const menu = Menu.buildFromTemplate([
    { label: isRunning ? '● Connected' : '○ Disconnected', enabled: false },
    { type: 'separator' },
    {
      label: 'Show Window',
      click: () => {
        mainWindow.show();
        mainWindow.focus();
      },
    },
    {
      label: isRunning ? 'Stop' : 'Start',
      click: () => {
        if (isRunning) stopAll();
        else mainWindow.show();
      },
    },
    { type: 'separator' },
    { label: 'Quit', click: () => { stopAll(); app.quit(); } },
  ]);
  tray.setContextMenu(menu);
}

// ── SSH Tunnel ───────────────────────────────────────────────────────────────

function startSSH(cfg) {
  return new Promise((resolve, reject) => {
    // Kill stale tunnels
    exec(`pkill -f "ssh.*-R ${cfg.tunnelPort}:localhost:${cfg.tunnelPort}"`, () => {
      setTimeout(() => {
        const args = [
          '-N',
          '-R', `${cfg.tunnelPort}:localhost:${cfg.tunnelPort}`,
          '-o', 'ExitOnForwardFailure=yes',
          '-o', 'ServerAliveInterval=30',
          '-o', 'ServerAliveCountMax=3',
          '-o', 'ConnectTimeout=10',
          '-o', 'StrictHostKeyChecking=accept-new',
          '-p', cfg.vpsPort,
          `${cfg.vpsUser}@${cfg.vpsHost}`,
        ];

        sshProcess = spawn('ssh', args, { stdio: ['ignore', 'pipe', 'pipe'] });
        let settled = false;

        const timer = setTimeout(() => {
          if (!settled) {
            settled = true;
            resolve(); // assume connected after 3s without error
          }
        }, 3000);

        sshProcess.stderr.on('data', (data) => {
          const msg = data.toString();
          sendLog(`[SSH] ${msg.trim()}`);
          if (!settled && msg.includes('Permission denied')) {
            settled = true;
            clearTimeout(timer);
            reject(new Error('SSH authentication failed. Set up SSH key: ssh-copy-id ' + cfg.vpsUser + '@' + cfg.vpsHost));
          }
        });

        sshProcess.on('close', (code) => {
          sendLog(`[SSH] tunnel closed (code ${code})`);
          if (!settled) {
            settled = true;
            clearTimeout(timer);
            reject(new Error(`SSH tunnel exited with code ${code}`));
          }
          sshProcess = null;
          if (isRunning) {
            isRunning = false;
            sendStatus('disconnected');
            updateTrayMenu();
          }
        });
      }, 500);
    });
  });
}

// ── mac-agent ────────────────────────────────────────────────────────────────

function startAgent(cfg) {
  return new Promise((resolve, reject) => {
    const extraPaths = ['/opt/homebrew/bin', '/usr/local/bin', '/opt/local/bin'];
    const currentPath = process.env.PATH || '';
    const env = {
      ...process.env,
      PATH: [...extraPaths, currentPath].join(':'),
      MAC_AGENT_PASSWORD: cfg.password,
      MAC_AGENT_TURN_URL: cfg.turnUrl,
      MAC_AGENT_TURN_USER: cfg.turnUser,
      MAC_AGENT_TURN_PASS: cfg.turnPass,
    };

    const args = [
      '-screen', cfg.screenIndex,
      '-fps', cfg.fps,
      '-width', cfg.width,
      '-bitrate', cfg.bitrate,
      '-addr', `127.0.0.1:${cfg.tunnelPort}`,
    ];

    agentProcess = spawn(binPath, args, { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let settled = false;

    agentProcess.stdout.on('data', (data) => {
      sendLog(data.toString().trim());
    });

    agentProcess.stderr.on('data', (data) => {
      const msg = data.toString().trim();
      sendLog(msg);
      if (!settled && msg.includes('listening on')) {
        settled = true;
        resolve();
      }
    });

    agentProcess.on('close', (code) => {
      sendLog(`[Agent] exited (code ${code})`);
      agentProcess = null;
      if (!settled) {
        settled = true;
        reject(new Error('mac-agent failed to start. Check permissions.'));
      }
      if (isRunning) {
        isRunning = false;
        sendStatus('disconnected');
        updateTrayMenu();
      }
    });

    // If no log after 5s, assume started (agent might log to stderr)
    setTimeout(() => {
      if (!settled && agentProcess && !agentProcess.killed) {
        settled = true;
        resolve();
      }
    }, 5000);
  });
}

// ── Start / Stop ─────────────────────────────────────────────────────────────

async function startAll(cfg) {
  try {
    sendStatus('connecting');
    sendLog('Opening SSH tunnel...');
    await startSSH(cfg);
    sendLog('SSH tunnel established');

    sendLog('Starting mac-agent...');
    await startAgent(cfg);
    sendLog('mac-agent is running');

    isRunning = true;
    sendStatus('connected');
    updateTrayMenu();
    return { ok: true };
  } catch (err) {
    stopAll();
    sendStatus('error');
    sendLog(`[Error] ${err.message}`);
    return { ok: false, error: err.message };
  }
}

function stopAll() {
  if (agentProcess) {
    agentProcess.kill();
    agentProcess = null;
  }
  if (sshProcess) {
    sshProcess.kill();
    sshProcess = null;
  }
  isRunning = false;
  sendStatus('disconnected');
  sendLog('Stopped');
  updateTrayMenu();
}

// ── IPC to renderer ──────────────────────────────────────────────────────────

function sendStatus(status) {
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.webContents.send('status', status);
  }
}

function sendLog(msg) {
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.webContents.send('log', msg);
  }
}

// ── IPC handlers ─────────────────────────────────────────────────────────────

ipcMain.handle('get-config', () => loadConfig());
ipcMain.handle('save-config', (_, cfg) => { saveConfig(cfg); return true; });
ipcMain.handle('get-status', () => isRunning ? 'connected' : 'disconnected');

ipcMain.handle('start', async (_, cfg) => {
  saveConfig(cfg);
  return startAll(cfg);
});

ipcMain.handle('stop', () => {
  stopAll();
  return true;
});

ipcMain.handle('check-binary', () => {
  return fs.existsSync(binPath);
});

ipcMain.handle('build-binary', () => {
  return new Promise((resolve) => {
    const buildDir = path.join(__dirname, '..');
    const outPath = path.join(__dirname, 'bin', 'mac-agent');

    // Ensure bin directory exists
    const binDir = path.dirname(outPath);
    if (!fs.existsSync(binDir)) fs.mkdirSync(binDir, { recursive: true });

    sendLog('Building mac-agent binary...');
    exec(
      `cd "${buildDir}" && CGO_ENABLED=1 go build -o "${outPath}" .`,
      { timeout: 120000 },
      (err, stdout, stderr) => {
        if (err) {
          sendLog(`[Build Error] ${stderr || err.message}`);
          resolve(false);
        } else {
          sendLog('Build complete');
          resolve(true);
        }
      }
    );
  });
});

// ── App lifecycle ────────────────────────────────────────────────────────────

app.whenReady().then(() => {
  createWindow();
  createTray();
});

app.on('window-all-closed', () => {
  if (!isRunning) app.quit();
});

app.on('activate', () => {
  if (mainWindow) mainWindow.show();
});

app.on('before-quit', () => {
  stopAll();
});
