import { spawn } from "node:child_process";
import { access, mkdir, readFile, rm } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const input = resolve(root, "..", "api", "openapi.yaml");
const output = resolve(root, "src", "api", "schema.gen.ts");
const bin = resolve(root, "node_modules", ".bin", process.platform === "win32" ? "openapi-typescript.cmd" : "openapi-typescript");

await access(input);
await access(bin);
await mkdir(dirname(output), { recursive: true });
const temp = `${output}.tmp-${process.pid}`;
await rm(temp, { force: true });
try {
  await new Promise((resolvePromise, reject) => {
    const child = spawn(bin, [input, "-o", temp], { cwd: root, stdio: "inherit", shell: false });
    child.once("error", reject);
    child.once("exit", (code, signal) => code === 0 ? resolvePromise() : reject(new Error(`openapi-typescript exited with ${code ?? signal}`)));
  });
  const generated = await readFile(temp, "utf8");
  if (!generated.trim()) throw new Error("generator produced an empty file");
  await (await import("node:fs/promises")).rename(temp, output);
} finally {
  await rm(temp, { force: true });
}
