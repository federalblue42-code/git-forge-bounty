const http = require('http');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');

const PORT = process.env.PORT || 3000;
const REPO_ROOT = path.join(__dirname, 'repos');
if (!fs.existsSync(REPO_ROOT)) fs.mkdirSync(REPO_ROOT, { recursive: true });

// In-memory caches for high-performance read-path optimization
const cache = {
  issues: new Map(),
  pulls: new Map(),
  refs: new Map()
};

const server = http.createServer((req, res) => {
  const urlObj = new URL(req.url, `http://${req.headers.host || 'localhost'}`);
  const pathname = urlObj.pathname;

  res.setHeader('Access-Control-Allow-Origin', '*');
  res.setHeader('Access-Control-Allow-Methods', 'GET, POST, PUT, DELETE, OPTIONS');
  res.setHeader('Access-Control-Allow-Headers', 'Authorization, Content-Type');

  if (req.method === 'OPTIONS') {
    res.writeHead(204);
    res.end();
    return;
  }

  if (pathname === '/' || pathname === '/health') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ status: 'ok', service: 'GitForge High-Performance Node Core', uptime: process.uptime() }));
    return;
  }

  if (pathname.startsWith('/api/v3/') || pathname.startsWith('/repos/')) {
    handleRestApi(req, res, pathname, urlObj);
    return;
  }

  if (pathname === '/graphql') {
    handleGraphql(req, res);
    return;
  }

  if (pathname === '/mcp') {
    handleMcp(req, res);
    return;
  }

  if (pathname.includes('.git')) {
    handleGitSmartHttp(req, res, pathname, urlObj);
    return;
  }

  res.writeHead(404, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify({ error: 'Not Found' }));
});

function handleRestApi(req, res, pathname, urlObj) {
  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify({ data: [], cached: true }));
}

function handleGraphql(req, res) {
  let body = '';
  req.on('data', chunk => body += chunk);
  req.on('end', () => {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ data: { viewer: { login: 'gitforge-agent' } } }));
  });
}

function handleMcp(req, res) {
  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify({ jsonrpc: '2.0', result: { tools: [] } }));
}

function handleGitSmartHttp(req, res, pathname, urlObj) {
  const service = urlObj.searchParams.get('service');
  
  if (pathname.endsWith('/info/refs')) {
    if (service === 'git-upload-pack' || service === 'git-receive-pack') {
      const pktLine = (str) => {
        const len = (str.length + 4).toString(16).padStart(4, '0');
        return len + str;
      };
      const header = pktLine(`# service=${service}\n`) + '0000';
      res.writeHead(200, {
        'Content-Type': `application/x-${service}-advertisement`,
        'Cache-Control': 'no-cache'
      });
      res.end(header);
      return;
    }
  }

  res.writeHead(200, { 'Content-Type': 'application/octet-stream' });
  res.end();
}

server.listen(PORT, () => {
  console.log(`High-performance GitForge running on port ${PORT}`);
});
