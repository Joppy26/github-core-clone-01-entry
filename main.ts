// Entry lifecycle for the forge: spawn the vendored static Go binary
// (`forge`, built from the Go sources in this repository) and let it own the
// harness lifecycle — bind an ephemeral port, POST /register, poll /done,
// exit 0. This file only spawns and forwards the exit code; all serving is
// in the binary. No dependencies, Node standard library only.
//
//   node /bot/main.ts   (the platform runs Node 22, which strips types)
//
// Without MAKEFUN_ENV_URL there is no env to register with, so exit 1, as
// the starter's main.ts does.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const forgePath = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

const child = spawn(forgePath, [], {
  stdio: 'inherit',
  env: process.env,
});

child.on('error', (err) => {
  process.stderr.write(`[entry] failed to start forge: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal !== null) {
    process.stderr.write(`[entry] forge ended on signal ${signal}\n`);
    process.exit(1);
  }
  process.exit(code ?? 1);
});
