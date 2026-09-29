#!/usr/bin/env node
// Validate every ```mermaid block in the given Markdown files/directories.
//
//   node scripts/check-mermaid.mjs docs README.md
//
// Uses the `mermaid` + `jsdom` packages already installed in frontend/ —
// run from the repo root (it resolves frontend/node_modules itself).
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const require = createRequire(join(root, "frontend", "package.json"));
const { JSDOM } = require("jsdom");

const dom = new JSDOM("<!DOCTYPE html><body></body>", { pretendToBeVisual: true });
globalThis.window = dom.window;
globalThis.document = dom.window.document;
if (!globalThis.navigator) globalThis.navigator = dom.window.navigator;

const mermaid = (await import(require.resolve("mermaid"))).default;
mermaid.initialize({ startOnLoad: false, suppressErrorRendering: true });

function* mdFiles(p) {
  const st = statSync(p);
  if (st.isDirectory()) {
    for (const e of readdirSync(p)) yield* mdFiles(join(p, e));
  } else if (p.endsWith(".md")) {
    yield p;
  }
}

const args = process.argv.slice(2);
if (args.length === 0) args.push("docs", "README.md");

let blocks = 0;
let failures = 0;
for (const arg of args) {
  for (const file of mdFiles(resolve(root, arg))) {
    const text = readFileSync(file, "utf8");
    const re = /```mermaid[^\n]*\n([\s\S]*?)```/g;
    let m;
    let idx = 0;
    while ((m = re.exec(text))) {
      idx++;
      blocks++;
      const line = text.slice(0, m.index).split("\n").length;
      try {
        await mermaid.parse(m[1]);
      } catch (err) {
        failures++;
        const msg = String(err?.message ?? err).split("\n").slice(0, 3).join(" | ");
        console.error(`✗ ${file.replace(root + "/", "")}:${line} (block #${idx}) — ${msg}`);
      }
    }
  }
}
console.log(`${blocks - failures}/${blocks} mermaid blocks OK`);
process.exit(failures ? 1 : 0);
