import { defineConfig } from "vite";
import { viteStaticCopy } from "vite-plugin-static-copy";
import path from "path";
import fs from "fs";

export default defineConfig({
    plugins: [
        viteStaticCopy({
            targets: [
                { src: "plugin.json", dest: "." },
                { src: "README.md", dest: "." },
                { src: "i18n/**", dest: "i18n" },
            ],
        }),
        {
            // 构建完成后把 index.cjs 复制为 index.js：
            // 思源内核（含 iOS APP）按 data/plugins/<name>/index.js 加载插件入口
            name: "rename-cjs-to-js",
            closeBundle() {
                fs.copyFileSync("dist/index.cjs", "dist/index.js");
            },
        },
    ],
    build: {
        outDir: "./dist",
        emptyOutDir: true,
        minify: false,
        rollupOptions: {
            external: ["siyuan", "process"],
        },
        lib: {
            entry: path.resolve(__dirname, "src/index.ts"),
            name: "SYNoteSync",
            fileName: "index",
            formats: ["cjs"],
        },
    },
});
