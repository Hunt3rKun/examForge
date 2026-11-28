#!/usr/bin/env bash
# Standalone, offline Web gate. Never installs dependencies or invokes npx.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../web"
fail() { printf 'verify-web: %s\n' "$1" >&2; exit 1; }

for tool in node npm; do
  command -v "$tool" >/dev/null 2>&1 || fail "required executable not found: $tool"
done
[[ -f package-lock.json ]] || fail 'web/package-lock.json is missing'
[[ -d node_modules ]] || fail 'web/node_modules is missing; provision locked dependencies before running this offline gate'
for tool in openapi-typescript tsc vitest vite; do
  [[ -x "node_modules/.bin/$tool" ]] || fail "web/node_modules/.bin/$tool is missing or not executable; provision locked dependencies first"
done

export npm_config_offline=true npm_config_audit=false npm_config_fund=false npm_config_update_notifier=false
node - "$(npm --version)" <<'NODE'
const { engines } = require('./package.json');
const version = /^(\d+)\.(\d+)\.(\d+)(?:\+[\w.-]+)?$/;
for (const [name, actual] of [['node', process.versions.node], ['npm', process.argv[2]]]) {
  const required = engines[name];
  const min = required?.startsWith('>=') && version.exec(required.slice(2));
  const found = version.exec(actual);
  if (!min) throw new Error(`verify-web: unsupported ${name} engine range: ${required}`);
  const delta = found && [1, 2, 3].map(i => Number(found[i]) - Number(min[i])).find(n => n !== 0);
  if (!found || delta < 0) throw new Error(`verify-web: ${name} ${required} is required, found ${actual}`);
}
NODE

# Cut networking at the OS level when possible. On hosts without unshare,
# keep the local Node tools from opening outgoing sockets instead.
offline=()
if command -v unshare >/dev/null 2>&1 && unshare -r -n true 2>/dev/null; then
  offline=(unshare -r -n --)
else
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "$tmpdir"' EXIT
  cat > "$tmpdir/no-network.cjs" <<'NODE'
const deny = () => { throw new Error('verify-web: networking is disabled'); };
require('node:net').Socket.prototype.connect = deny;
const udp = require('node:dgram').Socket.prototype;
udp.connect = udp.send = deny;
NODE
  export NODE_OPTIONS="${NODE_OPTIONS:+$NODE_OPTIONS }--require=\"$tmpdir/no-network.cjs\""
fi

"${offline[@]}" npm --ignore-scripts run typegen:check
# The root tsconfig has only project references: bare tsc --noEmit checks no files.
"${offline[@]}" ./node_modules/.bin/tsc --noEmit --project tsconfig.app.json
"${offline[@]}" ./node_modules/.bin/tsc --noEmit --project tsconfig.node.json
"${offline[@]}" ./node_modules/.bin/vitest run
"${offline[@]}" ./node_modules/.bin/vite build --emptyOutDir

node --input-type=module <<'NODE'
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { extname, join, posix } from 'node:path';

function fail(message) { throw new Error(`verify-web: ${message}`); }
function* walk(dir) {
  for (const entry of readdirSync(join('dist', dir), { withFileTypes: true })) {
    const path = posix.join(dir, entry.name);
    if (entry.isDirectory()) yield* walk(path);
    else if (entry.isFile()) yield path;
  }
}
const files = new Set(walk(''));
const read = path => readFileSync(join('dist', path), 'utf8');
const nonempty = path => files.has(path) && statSync(join('dist', path)).size > 0;
const html = read('index.html');
const scripts = [...files].filter(path => /\.[cm]?js$/.test(path));
const worker = scripts.find(path => /(?:^|\/)pdf\.worker-[^/]+\.mjs$/.test(path) && nonempty(path));
if (!worker || !scripts.some(path => path !== worker && read(path).includes(`/${worker}`))) {
  fail('dist is missing a referenced local pdf.js worker');
}
const appScripts = scripts.filter(path => path !== worker).map(read);
for (const [dir, extension] of [['cmaps', /\.bcmap$/], ['standard_fonts', /\.(?:ttf|pfb)$/]]) {
  if (!appScripts.some(text => text.includes(`${dir}/`))) continue;
  for (const entry of readdirSync(join('node_modules/pdfjs-dist', dir))) {
    if (extension.test(entry) && !nonempty(`pdfjs/${dir}/${entry}`)) {
      fail(`dist is missing local pdf.js resource pdfjs/${dir}/${entry}`);
    }
  }
}
const cssFiles = [...files].filter(path => extname(path) === '.css' && html.includes(`/${path}`));
let katex = false;
for (const path of cssFiles) {
  const css = read(path);
  for (const [face] of css.matchAll(/@font-face\s*\{[^}]*\}/gi)) {
    if (!/font-family:\s*["']?KaTeX_/.test(face)) continue;
    const refs = [...face.matchAll(/url\(\s*["']?([^)'"\s]+)/gi)].map(match => match[1]);
    if (!refs.length) fail(`missing KaTeX font references in ${path}`);
    for (const ref of refs) {
      if (ref.startsWith('data:font/')) continue;
      const target = posix.normalize(ref.startsWith('/') ? ref.slice(1) : posix.join(posix.dirname(path), ref));
      if (!nonempty(target)) fail(`missing local KaTeX font ${ref} in ${path}`);
    }
    katex = true;
  }
}
if (!katex) fail('dist is missing linked KaTeX CSS and fonts');

// The pinned dependencies contain XML namespace identifiers, a license URL,
// and an error-message URL. These exact strings are not runtime resources.
const namespaces = /^http:\/\/www\.w3\.org\/(?:1998\/Math\/MathML|1999\/(?:xhtml|xlink|XSL\/Transform)|2000\/(?:svg|09\/xmldsig#)|XML\/1998\/namespace)$/;
const workerMetadata = /^http:\/\/(?:www\.apache\.org\/licenses\/LICENSE-2\.0|ns\.adobe\.com\/(?:xdp\/(?:pdf\/)?|xfdf\/|xmpmeta\/)|www\.xfa\.org\/schema\/(?:xci|xdc|xfa-connection-set|xfa-data(?:\/1\.0)?|xfa-form|xfa-locale-set|xfa-source-set|xfa-template)\/)$/;
for (const path of files) {
  if (!/\.(?:html|css|[cm]?js|svg|xml|json|map|webmanifest|txt)$/i.test(path)) continue;
  const text = read(path);
  // Scan static HTTP(S) hosts and protocol-relative resource literals, not
  // JS comments, regex escapes, or network schemes built from document data.
  const urls = /https?:\/\/[a-z0-9%[\]][^\s"'`<>()\\]*|(?:[="'`(]\s*|(?:@import|\bsrcset)\s+)\/(\/[a-z0-9%[\]][^\s"'`<>()\\]*)/gi;
  for (const match of text.matchAll(urls)) {
    const url = match[1] ? `/${match[1]}` : match[0];
    if (scripts.includes(path) && (namespaces.test(url) || url === 'https://reactjs.org/docs/error-decoder.html?invariant=')) continue;
    if (path === worker && workerMetadata.test(url)) continue;
    if (/\.(?:svg|xml)$/.test(path) && namespaces.test(url)) continue;
    fail(`external URL in dist/${path}: ${url}`);
  }
}
NODE

printf 'verify-web: all checks passed\n'
