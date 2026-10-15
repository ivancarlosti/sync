#!/usr/bin/env node
/**
 * capture-screenshots.mjs — regenerates the screenshots the README shows.
 *
 * It drives the Chrome/Chromium already installed on the machine over the
 * DevTools Protocol (`--headless=new`), so the project keeps no browser
 * automation dependency. Two things happen:
 *
 *   1. the script signs in once through the API to obtain the session cookie the
 *      SPA uses (an instance running `AUTH_METHOD=account` renders nothing
 *      useful without it), and
 *   2. it visits every screen, waits until the SPA stopped issuing its own
 *      requests, and writes a full-page PNG at device pixel ratio 2 into
 *      `docs/screenshots`.
 *
 * The images show the demo dataset the "Interface" section of the README
 * describes: three connected accounts, four jobs, runs with their per-file
 * decisions, an audit report, a week of traffic and three notification channels.
 *
 * The files are written as truecolor PNGs. They are committed through an 8-bit
 * adaptive palette — the same look at about a third of the weight:
 *
 *   python3 -c "import sys; from PIL import Image; [Image.open(p).convert('RGB').quantize(colors=256, method=Image.MEDIANCUT).save(p, optimize=True) for p in sys.argv[1:]]" docs/screenshots/*.png
 *
 * Usage:
 *   npm run screenshots
 *   SCREENSHOT_BASE_URL=https://sync.example.com npm run screenshots
 *
 * Environment:
 *   SCREENSHOT_BASE_URL   origin of a running instance (default http://localhost:3000)
 *   SCREENSHOT_LOGIN      login of that instance      (default admin@example.com)
 *   SCREENSHOT_PASSWORD   its password                (default admin123)
 *   SCREENSHOT_DIR        output directory            (default ../../docs/screenshots)
 *   SCREENSHOT_DPR        device pixel ratio          (default 2)
 *   SCREENSHOT_MAX_HEIGHT tallest clip in CSS pixels  (default 2700)
 *   CHROME_BIN            browser to drive (default: google-chrome, then chromium)
 */
import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';

const BASE_URL = (process.env.SCREENSHOT_BASE_URL ?? 'http://localhost:3000').replace(/\/+$/, '');
const LOGIN = process.env.SCREENSHOT_LOGIN ?? 'admin@example.com';
const PASSWORD = process.env.SCREENSHOT_PASSWORD ?? 'admin123';
const OUT_DIR = resolve(
  process.env.SCREENSHOT_DIR ?? join(import.meta.dirname, '..', '..', 'docs', 'screenshots'),
);
const DPR = Number(process.env.SCREENSHOT_DPR ?? 2);
const MAX_HEIGHT = Number(process.env.SCREENSHOT_MAX_HEIGHT ?? 2700);
const VIEWPORT = { width: 1440, height: 900 };

/** chromeBinary returns the first browser the host provides. */
function chromeBinary() {
  if (process.env.CHROME_BIN) {
    return process.env.CHROME_BIN;
  }
  for (const name of ['google-chrome', 'google-chrome-stable', 'chromium', 'chromium-browser']) {
    for (const dir of ['/usr/bin/', '/usr/local/bin/', '/opt/google/chrome/']) {
      if (existsSync(dir + name)) {
        return dir + name;
      }
    }
  }
  throw new Error('no Chrome/Chromium found: set CHROME_BIN to the browser to drive');
}

/**
 * STEPS is the list of screens, in the order the README presents them. `title`
 * names the screen in the log, `select` fills native `<select>` fields before the
 * capture (the value an operator picks in the form) and `click` opens a detail
 * dialog: `row` picks the table row, `button` the label of the button inside it.
 * A dialog is captured as the viewport (its overlay covers the page), a plain
 * screen as the whole document.
 */
const STEPS = [
  { file: '01-login', path: '/login', title: 'Sign in' },
  { file: '02-dashboard', path: '/', title: 'Dashboard' },
  { file: '03-accounts', path: '/accounts', title: 'Accounts' },
  { file: '04-jobs', path: '/jobs', title: 'Jobs' },
  { file: '05-job-new', path: '/jobs/new', title: 'New job', select: { '#job-direction': 'google_to_microsoft', '#job-conflict': 'newest_wins' } },
  { file: '06-job-editor', path: '/jobs/1/edit', title: 'Job editor' },
  { file: '07-runs', path: '/runs', title: 'Runs' },
  {
    file: '08-run-detail',
    path: '/runs',
    title: 'Run detail',
    click: { row: 1, button: 'Files' },
    viewport: true,
  },
  { file: '09-audit', path: '/audit', title: 'Audit' },
  {
    file: '10-audit-report',
    path: '/audit',
    title: 'Audit report',
    click: { row: 0, button: 'Report' },
    viewport: true,
  },
  { file: '11-billing', path: '/billing', title: 'Billing' },
  { file: '12-notifications', path: '/notifications', title: 'Notifications' },
  { file: '13-providers', path: '/admin/providers', title: 'Administration — Providers' },
  { file: '14-setup-guide', path: '/admin/guide', title: 'Administration — Setup guide' },
  { file: '15-api-tokens', path: '/admin/api', title: 'Administration — API' },
  { file: '16-settings', path: '/admin/settings', title: 'Administration — Settings' },
  { file: '17-about', path: '/admin/about', title: 'Administration — About' },
  { file: '18-not-found', path: '/this-page-does-not-exist', title: 'Not found' },
];

// ---------------------------------------------------------------------------
// DevTools Protocol plumbing
// ---------------------------------------------------------------------------

/**
 * connect opens the browser level socket and returns a small CDP client. A new
 * tab is driven through a flat session, which keeps every request and event of
 * the capture routable to that one tab.
 */
async function connect(browserURL) {
  const socket = new WebSocket(browserURL);
  await new Promise((resolve_, reject) => {
    socket.addEventListener('open', resolve_, { once: true });
    socket.addEventListener('error', () => reject(new Error('the DevTools socket failed')), {
      once: true,
    });
  });

  const client = { socket, nextId: 1, pending: new Map(), listeners: [] };
  client.send = (method, params = {}, sessionId) => {
    const id = client.nextId++;
    const payload = { id, method, params };
    if (sessionId) {
      payload.sessionId = sessionId;
    }
    return new Promise((resolve_, reject) => {
      client.pending.set(id, { resolve: resolve_, reject });
      socket.send(JSON.stringify(payload));
    });
  };
  socket.addEventListener('message', (event) => {
    const message = JSON.parse(event.data);
    if (message.id && client.pending.has(message.id)) {
      const { resolve: resolve_, reject } = client.pending.get(message.id);
      client.pending.delete(message.id);
      if (message.error) {
        reject(new Error(`${message.error.message} (${message.error.code})`));
      } else {
        resolve_(message.result ?? {});
      }
      return;
    }
    for (const listener of client.listeners) {
      listener(message);
    }
  });
  return client;
}

/** waitForPort polls the DevTools port file Chrome writes on startup. */
async function waitForPort(profileDir, timeoutMs = 20000) {
  const file = join(profileDir, 'DevToolsActivePort');
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const [port] = (await readFile(file, 'utf8')).split('\n');
      if (port) {
        return `http://127.0.0.1:${Number(port.trim())}`;
      }
    } catch {
      /* Chrome has not written the file yet. */
    }
    await sleep(150);
  }
  throw new Error('Chrome did not open its DevTools port');
}

/** waitForTarget polls the HTTP endpoint for the WebSocket of the browser. */
async function browserWebSocket(host, timeoutMs = 20000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(`${host}/json/version`);
      const version = await response.json();
      if (version.webSocketDebuggerUrl) {
        return version.webSocketDebuggerUrl;
      }
    } catch {
      /* the HTTP endpoint is not up yet. */
    }
    await sleep(150);
  }
  throw new Error('the DevTools HTTP endpoint never answered');
}

// ---------------------------------------------------------------------------
// Capturing
// ---------------------------------------------------------------------------

/** signIn authenticates once and returns the Cookie header the SPA needs. */
async function signIn() {
  const response = await fetch(`${BASE_URL}/api/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ login: LOGIN, password: PASSWORD }),
  });
  if (!response.ok) {
    throw new Error(`sign in refused with ${response.status}: ${await response.text()}`);
  }
  const cookies = response.headers
    .getSetCookie()
    .map((cookie) => cookie.split(';')[0])
    .join('; ');
  if (cookies === '') {
    throw new Error('sign in returned no session cookie');
  }
  return cookies;
}

/**
 * READY is the expression that tells whether a screen finished rendering: the
 * SPA is mounted, no spinner is on screen and the document itself is complete.
 */
const READY = `(() => {
  const app = document.querySelector('#app');
  if (!app || app.innerHTML.trim() === '') return false;
  if (document.querySelector('.animate-spin')) return false;
  return document.readyState === 'complete';
})()`;

/** waitUntil polls an expression until it returns true. */
async function waitUntil(session, expression, timeoutMs = 15000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await session.evaluate(expression)) {
      return true;
    }
    await sleep(120);
  }
  return false;
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

async function main() {
  await mkdir(OUT_DIR, { recursive: true });
  const profile = join(process.env.TMPDIR ?? '/tmp', `sync-screenshots-${process.pid}`);
  await rm(profile, { recursive: true, force: true });

  console.log(`capturing ${STEPS.length} screens from ${BASE_URL} into ${OUT_DIR}`);
  const cookie = await signIn();
  const [cookieName, ...cookieRest] = cookie.split('=');

  const chrome = spawn(
    chromeBinary(),
    [
      '--headless=new',
      '--disable-gpu',
      '--hide-scrollbars',
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-extensions',
      `--user-data-dir=${profile}`,
      '--remote-debugging-port=0',
      'about:blank',
    ],
    // The browser runs in UTC, the timezone the API answers in (every timestamp
    // it returns is RFC 3339 `Z`), so a captured screen shows exactly what the
    // instance recorded.
    { stdio: ['ignore', 'ignore', 'pipe'], env: { ...process.env, TZ: 'UTC' } },
  );
  // Chrome is chatty on stderr (DBus, GPU probes); only the exit code matters.
  chrome.stderr.on('data', () => {});

  const host = await waitForPort(profile);
  const client = await connect(await browserWebSocket(host));

  try {
    const { targetId } = await client.send('Target.createTarget', { url: 'about:blank' });
    const { sessionId } = await client.send('Target.attachToTarget', { targetId, flatten: true });
    const session = {
      send: (method, params = {}) => client.send(method, params, sessionId),
      evaluate: async (expression) => {
        const answer = await session.send('Runtime.evaluate', {
          expression,
          returnByValue: true,
          awaitPromise: true,
        });
        if (answer.exceptionDetails) {
          throw new Error(answer.exceptionDetails.text ?? 'the page threw while evaluating');
        }
        return answer.result?.value;
      },
    };

    await session.send('Page.enable');
    await session.send('Runtime.enable');
    await session.send('Network.enable');
    await session.send('Emulation.setDeviceMetricsOverride', {
      width: VIEWPORT.width,
      height: VIEWPORT.height,
      deviceScaleFactor: DPR,
      mobile: false,
    });
    // The instance may default to `system`: pin light so every capture matches.
    await session.send('Emulation.setEmulatedMedia', {
      features: [{ name: 'prefers-color-scheme', value: 'light' }],
    });

    const written = [];
    for (const [index, step] of STEPS.entries()) {
      const label = `${String(index + 1).padStart(2, '0')}/${STEPS.length} ${step.file}`;
      if (index > 0) {
        // The first capture is the sign-in screen, which must not be signed in.
        await session.send('Network.setCookie', {
          name: cookieName,
          value: cookieRest.join('='),
          url: BASE_URL,
        });
      }
      await session.send('Page.navigate', { url: BASE_URL + step.path });
      const settled = await waitUntil(session, READY);
      await sleep(400);
      if (step.select) {
        const result = await session.evaluate(`(() => {
          const fields = ${JSON.stringify(step.select)};
          for (const [selector, value] of Object.entries(fields)) {
            const field = document.querySelector(selector);
            if (!field) return 'no field ' + selector;
            field.value = value;
            field.dispatchEvent(new Event('change', { bubbles: true }));
          }
          return 'chosen';
        })()`);
        if (result !== 'chosen') {
          throw new Error(`${step.file}: ${result}`);
        }
        await sleep(200);
      }
      if (step.click) {
        const result = await session.evaluate(`(() => {
          const rows = document.querySelectorAll('table tbody tr');
          const row = rows[${step.click.row}];
          if (!row) return 'no row ${step.click.row}';
          const button = [...row.querySelectorAll('button')].find(
            (candidate) => candidate.textContent.trim() === ${JSON.stringify(step.click.button)},
          );
          if (!button) return 'the button was not found';
          button.click();
          return 'clicked';
        })()`);
        if (result !== 'clicked') {
          throw new Error(`${step.file}: ${result}`);
        }
        if (!(await waitUntil(session, `!!document.querySelector('[role="dialog"]')`))) {
          throw new Error(`${step.file}: the dialog never opened`);
        }
        await sleep(700);
      }

      const metrics = await session.send('Page.getLayoutMetrics');
      const content = metrics.cssContentSize ?? metrics.contentSize;
      const height = step.viewport
        ? VIEWPORT.height
        : Math.min(Math.ceil(content.height), MAX_HEIGHT);
      const shot = await session.send('Page.captureScreenshot', {
        format: 'png',
        captureBeyondViewport: true,
        clip: { x: 0, y: 0, width: VIEWPORT.width, height, scale: 1 },
      });
      const bytes = Buffer.from(shot.data, 'base64');
      await writeFile(join(OUT_DIR, `${step.file}.png`), bytes);
      written.push(bytes.length);
      console.log(
        `  ${label.padEnd(24)} ${step.title.padEnd(28)} ` +
          `${String(Math.round(bytes.length / 1024)).padStart(5)} KiB  ` +
          `${VIEWPORT.width}x${height}@${DPR}x${settled ? '' : '  (timed out waiting)'}`,
      );
    }

    const total = written.reduce((sum, size) => sum + size, 0);
    console.log(
      `wrote ${written.length} screenshots, ${(total / 1024 / 1024).toFixed(2)} MiB in total`,
    );
  } finally {
    client.socket.close();
    chrome.kill('SIGTERM');
    // Chrome takes a moment to flush its profile: give it one, then drop it.
    await sleep(500);
    await rm(profile, { recursive: true, force: true, maxRetries: 5 }).catch(() => {});
  }
}

main().catch((error) => {
  console.error(`screenshot capture failed: ${error.message}`);
  process.exitCode = 1;
});


