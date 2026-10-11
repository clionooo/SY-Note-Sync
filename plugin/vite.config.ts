import { defineConfig } from "vite";
import { viteStaticCopy } from "vite-plugin-static-copy";
import path from "path";
import fs from "fs";

/**
 * 构建说明（两个容易踩的点）：
 *  1. 思源内核按 data/plugins/<name>/index.js 加载插件入口，
 *     而 vite 的 cjs 产物叫 index.cjs —— 构建完要复制一份改名。
 *  2. 思源只加载 index.css，vite 默认产物是 style.css —— 同样要改名，
 *     否则样式会被静默丢弃（设置面板会很难看，且没有任何报错）。
 */
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
            name: "siyuan-petal-filenames",
            closeBundle() {
                if (fs.existsSync("dist/index.cjs")) {
                    fs.copyFileSync("dist/index.cjs", "dist/index.js");
                }
                if (fs.existsSync("dist/style.css")) {
                    fs.copyFileSync("dist/style.css", "dist/index.css");
                }
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
