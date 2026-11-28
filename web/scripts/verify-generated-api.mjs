import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const committed = resolve(root, "src", "api", "schema.gen.ts");
const input = resolve(root, "..", "api", "openapi.yaml");
const bin = resolve(root, "node_modules", ".bin", process.platform === "win32" ? "openapi-typescript.cmd" : "openapi-typescript");
const tempDir = await mkdtemp(resolve(tmpdir(), "examforge-api-types-"));
const generated = resolve(tempDir, "schema.gen.ts");
try {
  await new Promise((ok, fail) => {
    const child = spawn(bin, [input, "-o", generated], { cwd: root, stdio: "inherit", shell: false });
    child.once("error", fail);
    child.once("exit", code => code === 0 ? ok() : fail(new Error(`openapi-typescript exited with ${code}`)));
  });
  const [expected, actual] = await Promise.all([readFile(committed), readFile(generated)]);
  if (!expected.equals(actual)) throw new Error("generated API types are stale; run npm run generate-api-types");
} finally {
  await rm(tempDir, { recursive: true, force: true });
}
