const fs = require('fs');
const path = require('path');
const os = require('os');

const GH_TOKEN_FILE = path.join(os.homedir(), '.xmuggle', 'gh-token');

function getGhToken() {
  if (process.env.GH_TOKEN) return process.env.GH_TOKEN;
  if (process.env.GITHUB_TOKEN) return process.env.GITHUB_TOKEN;
  try {
    return fs.readFileSync(GH_TOKEN_FILE, 'utf8').trim();
  } catch {
    return null;
  }
}

function setGhToken(token) {
  const dir = path.dirname(GH_TOKEN_FILE);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(GH_TOKEN_FILE, token.trim() + '\n', { mode: 0o600 });
}

function resetGhToken() {
  try { fs.unlinkSync(GH_TOKEN_FILE); } catch {}
}

function hasGhToken() {
  return !!getGhToken();
}

function gitEnv() {
  const token = getGhToken();
  if (!token) return process.env;

  // Create a tiny script that prints the token when git asks for a password.
  // GIT_ASKPASS is invoked with a prompt argument; we ignore it and just print the token.
  const askpass = path.join(os.tmpdir(), 'xmuggle-askpass.sh');
  fs.writeFileSync(askpass, `#!/bin/sh\nexec echo '${token.replace(/'/g, "'\\''")}'\n`, { mode: 0o700 });

  return {
    ...process.env,
    GIT_ASKPASS: askpass,
    GIT_TERMINAL_PROMPT: '0',
  };
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

module.exports = { getGhToken, setGhToken, resetGhToken, hasGhToken, getRelayHost, setRelayHost, gitEnv };
