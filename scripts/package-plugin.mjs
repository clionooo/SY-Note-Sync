#!/usr/bin/env node
/**
 * 把 plugin/dist 打成可以直接分发的 zip。
 *
 * 产物：
 *   dist/sy-note-sync-v<版本>.zip          —— 思源插件包（解压到 data/plugins/sy-note-sync/）
 *   dist/sy-note-sync-v<版本>-ios.zip      —— 同上，专供 iOS 通过「文件」App 导入
 *
 * 纯 Node 标准库实现（用系统 zip 命令），不引入额外依赖。
 */
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, "..");
const pluginDir = path.join(root, "plugin");
const distDir = path.join(pluginDir, "dist");
const outDir = path.join(root, "dist");

if (!fs.existsSync(distDir)) {
    console.error("找不到 plugin/dist，请先执行 npm run build");
    process.exit(1);
}
fs.mkdirSync(outDir, { recursive: true });

const manifest = JSON.parse(fs.readFileSync(path.join(pluginDir, "plugin.json"), "utf8"));
const version = manifest.version || "0.0.0";
const base = `sy-note-sync-v${version}`;

/** 在临时目录里摆好 <name>/ 结构再压缩，解压出来就是思源要的目录名 */
function pack(target) {
    const stage = fs.mkdtempSync(path.join(process.env.TMPDIR || "/tmp", "sns-pack-"));
    const inner = path.join(stage, "sy-note-sync");
    fs.mkdirSync(inner, { recursive: true });
    fs.cpSync(distDir, inner, { recursive: true });
    const zipPath = path.join(outDir, target);
    fs.rmSync(zipPath, { force: true });
    execFileSync("zip", ["-rq", zipPath, "sy-note-sync"], { cwd: stage });
    fs.rmSync(stage, { recursive: true, force: true });
    return zipPath;
}

const a = pack(`${base}.zip`);
const b = pack(`${base}-ios.zip`);
console.log("已生成：");
console.log("  " + a);
console.log("  " + b);
