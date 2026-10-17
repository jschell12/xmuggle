const fs = require('fs');
const path = require('path');
const os = require('os');

function gitEnv() {
  // Use scredmgr-injected environment and Git credential helpers, without token files.
  return process.env;
}

// ── Relay host ──

const RELAY_FILE = path.join(os.homedir(), '.xmuggle', 'relay-host');

function getRelayHost() {
  try { return fs.readFileSync(RELAY_FILE, 'utf8').trim(); } catch { return ''; }
}

function setRelayHost(host) {
  fs.mkdirSync(path.dirname(RELAY_FILE), { recursive: true });
  fs.writeFileSync(RELAY_FILE, host.trim() + '\n');
}

module.exports = { getRelayHost, setRelayHost, gitEnv };
