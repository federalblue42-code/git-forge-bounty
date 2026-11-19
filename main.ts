// github-core-clone-01 entrypoint (Node 22).
// The forge core is a statically linked Go binary vendored in this repository
// (source under core/). This launcher picks the binary for the host architecture,
// spawns it as the one forge process, and mirrors its exit code.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { chmodSync } from 'node:fs';

const here = dirname(fileURLToPath(import.meta.url));
const arch = process.arch === 'arm64' ? 'arm64' : 'amd64';
const bin = join(here, `forge-linux-${arch}`);
try { chmodSync(bin, 0o755); } catch { /* read-only mount keeps the committed mode */ }

const child = spawn(bin, [], {
  stdio: ['ignore', 'inherit', 'inherit'],
  env: process.env,
});
child.on('exit', (code, signal) => {
  if (signal) process.kill(process.pid, signal);
  else process.exit(code ?? 0);
});
child.on('error', (err) => {
  console.error(`forge: failed to start ${bin}: ${err.message}`);
  process.exit(1);
});