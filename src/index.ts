import { Plugin, showMessage } from "siyuan";
import "./index.css";

export default class SYNoteSync extends Plugin {
    async onload() {
        console.log("SY Note Sync loaded");
        this.addCommand({
            langKey: "syncNow",
            hotkey: "⌥⌘S",
            callback: () => this.syncNow(),
        });
    }

    async onLayoutReady() {
        // TODO: 初始化同步逻辑（等待具体方案）
    }

    private async syncNow() {
        // TODO: 实现同步核心逻辑
        showMessage("SY Note Sync: sync started");
    }
}
