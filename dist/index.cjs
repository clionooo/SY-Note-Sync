"use strict";
const siyuan = require("siyuan");
class SYNoteSync extends siyuan.Plugin {
  async onload() {
    console.log("SY Note Sync loaded");
    this.addCommand({
      langKey: "syncNow",
      hotkey: "⌥⌘S",
      callback: () => this.syncNow()
    });
  }
  async onLayoutReady() {
  }
  async syncNow() {
    siyuan.showMessage("SY Note Sync: sync started");
  }
}
module.exports = SYNoteSync;
