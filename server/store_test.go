// 存储层单元测试：验证 SQLite 表结构、设备注册、变更写入、冲突记录、快照
// 运行：cd server && go test -v ./...
package main

import (
	"testing"
)

// setupTestDB 每个用例使用独立内存数据库（:memory:），互不干扰
func setupTestDB(t *testing.T) {
	t.Helper()
	if err := initDB(":memory:"); err != nil {
		t.Fatalf("初始化测试数据库失败: %v", err)
	}
}

// TestInitDB 验证所有表都能正常创建
func TestInitDB(t *testing.T) {
	setupTestDB(t)
	// 重复初始化不应报错（CREATE TABLE IF NOT EXISTS 幂等）
	if err := initDB(":memory:"); err != nil {
		t.Fatalf("重复初始化应幂等: %v", err)
	}
}

// TestDeviceRegister 设备注册与密钥鉴权查询
func TestDeviceRegister(t *testing.T) {
	setupTestDB(t)
	id, key, err := createDevice("测试设备")
	if err != nil {
		t.Fatalf("注册设备失败: %v", err)
	}
	if key == "" || len(key) != 32 {
		t.Fatalf("设备密钥应为 32 位十六进制, got: %q", key)
	}
	// 密钥可反查设备
	gotID, gotName, err := deviceByKey(key)
	if err != nil || gotID != id || gotName != "测试设备" {
		t.Fatalf("密钥反查设备失败: id=%d name=%s err=%v", gotID, gotName, err)
	}
	// 错误密钥应查不到
	if _, _, err := deviceByKey("wrong-key"); err == nil {
		t.Fatal("错误密钥不应查到设备")
	}
}

// TestChangeLifecycle 变更写入 → 最新块状态更新 → 全量读取
func TestChangeLifecycle(t *testing.T) {
	setupTestDB(t)
	devID, _, _ := createDevice("dev1")

	c := &BlockChange{BlockID: "blk1", DocID: "doc1", Op: "insert", Markdown: "# hello", ParentID: "doc1"}
	rev, err := insertChange(c, devID)
	if err != nil {
		t.Fatalf("写变更失败: %v", err)
	}
	if rev == 0 {
		t.Fatal("变更版本号应大于 0")
	}

	applyChange(c, devID, rev)

	// 全量读取应包含该块
	blocks, err := getFullBlocks(0)
	if err != nil {
		t.Fatalf("全量读取失败: %v", err)
	}
	found := false
	for _, b := range blocks {
		if b.BlockID == "blk1" && b.Markdown == "# hello" && b.Rev == rev {
			found = true
		}
	}
	if !found {
		t.Fatal("全量结果中未找到刚写入的块")
	}

	// 同块再更新一次：blocks 表应只保留最新
	c2 := &BlockChange{BlockID: "blk1", DocID: "doc1", Op: "update", Markdown: "# hello v2"}
	rev2, _ := insertChange(c2, devID)
	applyChange(c2, devID, rev2)
	blocks2, _ := getFullBlocks(0)
	if len(blocks2) != 1 {
		t.Fatalf("同块更新后应只有 1 条最新状态, got %d", len(blocks2))
	}
	if blocks2[0].Markdown != "# hello v2" {
		t.Fatalf("应保留最新内容, got: %s", blocks2[0].Markdown)
	}
}

// TestDeleteOp 删除操作应把块从最新状态表移除
func TestDeleteOp(t *testing.T) {
	setupTestDB(t)
	devID, _, _ := createDevice("dev")
	c := &BlockChange{BlockID: "blk-del", DocID: "d1", Op: "insert", Markdown: "x"}
	rev, _ := insertChange(c, devID)
	applyChange(c, devID, rev)

	d := &BlockChange{BlockID: "blk-del", DocID: "d1", Op: "delete"}
	revD, _ := insertChange(d, devID)
	applyChange(d, devID, revD)

	blocks, _ := getFullBlocks(0)
	for _, b := range blocks {
		if b.BlockID == "blk-del" {
			t.Fatal("删除后的块不应出现在最新状态表")
		}
	}
}

// TestConflictFlow 冲突创建 → 读取 → 版本详情
func TestConflictFlow(t *testing.T) {
	setupTestDB(t)
	devA, _, _ := createDevice("A")
	devB, _, _ := createDevice("B")

	ca := &BlockChange{BlockID: "blkX", DocID: "d1", Op: "update", Markdown: "A版"}
	revA, _ := insertChange(ca, devA)
	applyChange(ca, devA, revA)

	// B 设备带旧 baseRev 推送 → 应记录冲突
	cb := &BlockChange{BlockID: "blkX", DocID: "d1", Op: "update", Markdown: "B版", BaseRev: 0}
	revB, _ := insertChange(cb, devB)
	cid, err := createConflict("blkX", "d1", revA, revB)
	if err != nil {
		t.Fatalf("创建冲突失败: %v", err)
	}
	if cid == 0 {
		t.Fatal("冲突 ID 应大于 0")
	}

	// 两个版本详情都能读回
	va, err := getChangeByID(revA)
	if err != nil || va.Markdown != "A版" {
		t.Fatalf("读取版本A失败: %v %+v", err, va)
	}
	vb, err := getChangeByID(revB)
	if err != nil || vb.Markdown != "B版" {
		t.Fatalf("读取版本B失败: %v %+v", err, vb)
	}
}

// TestSnapshotLifecycle 快照创建 → 列表 → 按快照读取历史状态
func TestSnapshotLifecycle(t *testing.T) {
	setupTestDB(t)
	devID, _, _ := createDevice("dev")

	c := &BlockChange{BlockID: "blk1", DocID: "d1", Op: "insert", Markdown: "快照时内容"}
	rev, _ := insertChange(c, devID)
	applyChange(c, devID, rev)

	sid, err := createSnapshot("测试快照")
	if err != nil {
		t.Fatalf("创建快照失败: %v", err)
	}

	// 快照后再修改块
	c2 := &BlockChange{BlockID: "blk1", DocID: "d1", Op: "update", Markdown: "修改后的内容"}
	rev2, _ := insertChange(c2, devID)
	applyChange(c2, devID, rev2)

	// 快照内容应保持创建时刻的状态
	snapBlocks, err := getFullBlocks(sid)
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if len(snapBlocks) != 1 || snapBlocks[0].Markdown != "快照时内容" {
		t.Fatalf("快照应保留历史内容, got: %+v", snapBlocks)
	}

	// 快照列表应包含该快照且计数为 1 块
	list, err := listSnapshots()
	if err != nil || len(list) != 1 || list[0].Blocks != 1 {
		t.Fatalf("快照列表异常: %+v err=%v", list, err)
	}
}
