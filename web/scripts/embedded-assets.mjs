#!/usr/bin/env node

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const webRoot = path.resolve(scriptDir, "..");
const repoRoot = path.resolve(webRoot, "..");
const sourceDir = path.join(webRoot, "dist");
const targetDir = path.join(repoRoot, "server", "internal", "webui", "dist");

const mode = process.argv[2];

if (!["check", "sync"].includes(mode)) {
  console.error("Usage: npm run embedded:check|embedded:sync");
  process.exit(2);
}

async function pathType(filePath) {
  try {
    const stat = await fs.lstat(filePath);
    if (stat.isDirectory()) {
      return "directory";
    }
    if (stat.isFile()) {
      return "file";
    }
    return "other";
  } catch (err) {
    if (err?.code === "ENOENT") {
      return "missing";
    }
    throw err;
  }
}

async function compareDirs(left, right, relative = "") {
  const leftType = await pathType(left);
  const rightType = await pathType(right);
  const label = relative || ".";

  if (leftType !== rightType) {
    return `${label}: ${leftType} != ${rightType}`;
  }
  if (leftType === "missing") {
    return `${label}: missing`;
  }
  if (leftType === "other") {
    return `${label}: unsupported file type`;
  }
  if (leftType === "file") {
    const [leftBytes, rightBytes] = await Promise.all([fs.readFile(left), fs.readFile(right)]);
    if (!leftBytes.equals(rightBytes)) {
      return `${label}: contents differ`;
    }
    return "";
  }

  const [leftEntries, rightEntries] = await Promise.all([fs.readdir(left), fs.readdir(right)]);
  leftEntries.sort();
  rightEntries.sort();
  if (leftEntries.join("\0") !== rightEntries.join("\0")) {
    return `${label}: entries differ (${leftEntries.join(", ")}) != (${rightEntries.join(", ")})`;
  }

  for (const entry of leftEntries) {
    const mismatch = await compareDirs(
      path.join(left, entry),
      path.join(right, entry),
      path.join(relative, entry),
    );
    if (mismatch) {
      return mismatch;
    }
  }
  return "";
}

if (mode === "sync") {
  await fs.rm(targetDir, { recursive: true, force: true });
  await fs.mkdir(path.dirname(targetDir), { recursive: true });
  await fs.cp(sourceDir, targetDir, { recursive: true });
  console.log(`Synced ${path.relative(repoRoot, sourceDir)} to ${path.relative(repoRoot, targetDir)}`);
} else {
  const mismatch = await compareDirs(sourceDir, targetDir);
  if (mismatch) {
    console.error(`Embedded Web UI assets are stale: ${mismatch}`);
    console.error("Run `cd web && npm run build && npm run embedded:sync`, then commit server/internal/webui/dist.");
    process.exit(1);
  }
  console.log("Embedded Web UI assets are up to date.");
}
