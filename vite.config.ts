import { defineConfig } from "vite";
import { viteStaticCopy } from "vite-plugin-static-copy";
import path from "path";

export default defineConfig({
    plugins: [
        viteStaticCopy({
            targets: [
                { src: "plugin.json", dest: "." },
                { src: "README.md", dest: "." },
                { src: "i18n/**", dest: "i18n" },
            ],
        }),
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
