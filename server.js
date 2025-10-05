import express from "express";
import fetch from "node-fetch";
import cors from "cors";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

// ===== Configuration (prefer env; do NOT hardcode secrets) =====
const UDR_BASE = process.env.UDR_BASE || "https://192.168.69.1";
let   SITE_ID  = process.env.UNIFI_SITE_ID || "88f7af54-98f8-306a-a1c7-c9349722b1f6";
const API_KEY  = process.env.UNIFI_API_KEY; // no fallback: avoid committing secrets
const PORT     = process.env.PORT || 5173;
const UNSAFE_TLS = process.env.UNSAFE_TLS === '1'; // set to 1 only if you cannot install the UDR CA

if (!API_KEY) {
  console.error("[FATAL] UNIFI_API_KEY is not set. Export it in your environment.");
  process.exit(1);
}

const app = express();
app.use(cors());
app.use(express.json({ limit: '256kb' }));
app.use(express.static(path.join(__dirname, 'public')));

// Reusable HTTPS agent. In dev you can set UNSAFE_TLS=1 to skip TLS validation; prefer installing CA.
let httpsAgent;
(async () => {
  const https = await import('https');
  httpsAgent = new https.Agent({ keepAlive: true, rejectUnauthorized: !UNSAFE_TLS });
})();

// Generic request with timeout
async function doRequest(method, pathname, body) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 10000); // 10s timeout
  const url = `${UDR_BASE}/proxy/network/integration/v1${pathname}`;
  try {
    const res = await fetch(url, {
      method,
      headers: {
        'X-API-KEY': API_KEY,
        'Accept': 'application/json',
        ...(body ? { 'Content-Type': 'application/json' } : {})
      },
      body: body ? JSON.stringify(body) : undefined,
      agent: httpsAgent,
      signal: controller.signal
    });
    if (!res.ok) {
      const text = await res.text().catch(() => '');
      throw new Error(`${res.status} ${res.statusText}${text ? `: ${text}` : ''}`);
    }
    return res.json();
  } finally {
    clearTimeout(timer);
  }
}

const udrGet  = (p) => doRequest('GET',  p);
const udrPost = (p,b) => doRequest('POST', p, b);

// Map Integration v1 site UUID -> legacy short name (e.g., "default")
async function getLegacySiteName(siteId){
  try{
    const sites = await udrGet('/sites');
    const arr = Array.isArray(sites?.data) ? sites.data : [];
    const m = arr.find(s => s.id === siteId);
    if (m && m.internalReference) return m.internalReference;
    // fallback: first site or 'default'
    return (arr[0] && arr[0].internalReference) ? arr[0].internalReference : 'default';
  }catch{
    return 'default';
  }
}

// ---- Legacy health endpoint (WAN instantaneous rates) ----
// Many UniFi builds expose rx_bytes-r / tx_bytes-r on /api/s/<site>/stat/health under the 'wan' subsystem.
// We go through the same console proxy but use the legacy path (not Integration v1).
async function udrGetWanHealth(siteId){
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 8000);
  const legacySite = await getLegacySiteName(siteId);
  const url = `${UDR_BASE}/proxy/network/api/s/${encodeURIComponent(legacySite)}/stat/health`;
  try {
    // 1) Try with API key header
    let res = await fetch(url, {
      method: 'GET',
      headers: { 'Accept': 'application/json', 'X-API-KEY': API_KEY },
      agent: httpsAgent,
      signal: controller.signal
    });
    // 2) If forbidden/unauthorized, retry WITHOUT the key header (some UniFi builds require legacy auth here)
    if (res.status === 401 || res.status === 403) {
      res = await fetch(url, {
        method: 'GET',
        headers: { 'Accept': 'application/json' },
        agent: httpsAgent,
        signal: controller.signal
      });
    }
    if (!res.ok) {
      const text = await res.text().catch(() => '');
      const err = new Error(`${res.status} ${res.statusText}${text ? `: ${text}` : ''}`);
      err.status = res.status;
      throw err;
    }
    return res.json();
  } finally {
    clearTimeout(timer);
  }
}

// Current WAN rates (bytes/sec) + basic status
app.get('/api/wan/health', async (req, res) => {
  try {
    const siteId = (req.query.siteId && String(req.query.siteId)) || SITE_ID;
    const raw = await udrGetWanHealth(siteId);
    const arr = Array.isArray(raw?.data) ? raw.data : [];
    const wan = arr.find(x => x && x.subsystem === 'wan') || {};
    const rx = typeof wan['rx_bytes-r'] === 'number' ? wan['rx_bytes-r'] : null;
    const tx = typeof wan['tx_bytes-r'] === 'number' ? wan['tx_bytes-r'] : null;
    res.json({
      ts: Date.now(),
      rx_bps: rx,
      tx_bps: tx,
      wan_ip: wan.wan_ip || null,
      status: wan.status || null,
      legacy_site: await getLegacySiteName(siteId),
      note: (!rx && !tx) ? 'no_wan_rate_data' : undefined
    });
  } catch (e) {
    res.status(502).json({ error: String(e) });
  }
});

// ========== API exposed to the frontend ==========

// Simple health endpoint for your reverse proxy/monitoring
app.get('/health', (_req, res) => res.json({ ok: true }));

// List sites
app.get('/api/sites', async (_req, res) => {
  try { res.json(await udrGet('/sites')); }
  catch (e) { res.status(500).json({ error: String(e) }); }
});

// Allow the UI to change default site (kept in-process)
app.post('/api/site', (req, res) => {
  const siteId = (req.body && String(req.body.siteId || '').trim()) || '';
  if (!siteId) return res.status(400).json({ error: 'siteId required' });
  SITE_ID = siteId;
  res.json({ ok: true, siteId });
});

// List clients for a site (optional ?siteId= overrides default)
app.get('/api/clients', async (req, res) => {
  try {
    const siteId = (req.query.siteId && String(req.query.siteId)) || SITE_ID;
    res.json(await udrGet(`/sites/${encodeURIComponent(siteId)}/clients`));
  } catch (e) { res.status(500).json({ error: String(e) }); }
});

// List devices for a site
app.get('/api/devices', async (req, res) => {
  try {
    const siteId = (req.query.siteId && String(req.query.siteId)) || SITE_ID;
    res.json(await udrGet(`/sites/${encodeURIComponent(siteId)}/devices`));
  } catch (e) { res.status(500).json({ error: String(e) }); }
});

// Authorize guest client (External Hotspot action)
app.post('/api/clients/:clientId/authorize', async (req, res) => {
  try {
    const siteId = (req.query.siteId && String(req.query.siteId)) || SITE_ID;
    const clientId = String(req.params.clientId || '').trim();
    if (!clientId) return res.status(400).json({ error: 'clientId required' });
    const payload = {
      action: 'AUTHORIZE_GUEST_ACCESS',
      timeLimitMinutes: req.body?.timeLimitMinutes ?? 120,
      dataUsageLimitMBytes: req.body?.dataUsageLimitMBytes ?? 0,
      txRateLimitKbps: req.body?.txRateLimitKbps ?? 0,
      rxRateLimitKbps: req.body?.rxRateLimitKbps ?? 0
    };
    const out = await udrPost(`/sites/${encodeURIComponent(siteId)}/clients/${encodeURIComponent(clientId)}/actions`, payload);
    res.json(out);
  } catch (e) { res.status(500).json({ error: String(e) }); }
});

app.listen(PORT, () => {
  console.log(`UI running: http://localhost:${PORT}`);
  console.log(`Using UDR base ${UDR_BASE} · default site ${SITE_ID} · UNSAFE_TLS=${UNSAFE_TLS ? 'ON' : 'OFF'}`);
});
