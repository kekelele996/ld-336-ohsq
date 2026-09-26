package service

import (
	"net/http"
	"testing"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/dto"
	"github.com/medasset/medasset/internal/model"
	"github.com/medasset/medasset/internal/repository"
	"github.com/medasset/medasset/internal/util"
)

func newRepairFlowEnv(t *testing.T) (*testEnv, *MaintenanceService, *TransferService, *ScrapService, *DeviceService) {
	t.Helper()
	env := newTestServiceEnv(t)
	deviceRepo := repository.NewDeviceRepository(env.db)
	maintRepo := repository.NewMaintenanceRepository(env.db)
	maintSvc := NewMaintenanceService(maintRepo, deviceRepo, env.audit, env.logger)
	transferSvc := NewTransferService(repository.NewTransferRepository(env.db), deviceRepo, maintRepo, env.audit, env.logger)
	scrapSvc := NewScrapService(repository.NewScrapRepository(env.db), deviceRepo, maintRepo, env.audit, env.logger)
	deviceSvc := NewDeviceService(deviceRepo, env.audit, env.logger)
	return env, maintSvc, transferSvc, scrapSvc, deviceSvc
}

func createTestDevice(t *testing.T, svc *DeviceService, assetCode string) *model.Device {
	t.Helper()
	d, err := svc.Create(&dto.CreateDeviceReq{AssetCode: assetCode, Name: "监护仪", Category: "生命支持", Department: "ICU", Status: constants.DeviceStatusInUse}, "admin")
	if err != nil {
		t.Fatalf("create device failed: %v", err)
	}
	return d
}

func TestRepairCreateStoresPreviousStatusAndLocksDevice(t *testing.T) {
	_, maintSvc, transferSvc, scrapSvc, deviceSvc := newRepairFlowEnv(t)
	d := createTestDevice(t, deviceSvc, "RP-001")

	// 维修前先存在一笔待审批调拨。
	tr, err := transferSvc.Create(&dto.CreateTransferReq{DeviceID: d.ID, ToDepartment: "急诊科", ToPerson: "张三"}, "dept01")
	if err != nil {
		t.Fatalf("create transfer failed: %v", err)
	}

	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{
		DeviceID: d.ID, Type: constants.MaintenanceTypeRepair, FaultDescription: "无法开机",
	}, "engineer01")
	if err != nil {
		t.Fatalf("create repair failed: %v", err)
	}
	if m.PreviousStatus != constants.DeviceStatusInUse {
		t.Errorf("previous_status = %q, want %q", m.PreviousStatus, constants.DeviceStatusInUse)
	}
	got, _ := deviceSvc.repo.FindByID(d.ID)
	if got.Status != constants.DeviceStatusUnderMaintenance {
		t.Errorf("device status = %q, want under_maintenance", got.Status)
	}

	// 维修中再次创建维修单应被拦截。
	if _, err := maintSvc.Create(&dto.CreateMaintenanceReq{
		DeviceID: d.ID, Type: constants.MaintenanceTypeRepair,
	}, "engineer01"); !isConflict(err) {
		t.Errorf("second repair create err = %v, want conflict", err)
	}

	// 维修中新建调拨、报废均应被挡住。
	if _, err := transferSvc.Create(&dto.CreateTransferReq{DeviceID: d.ID, ToDepartment: "口腔科"}, "dept01"); !isConflict(err) {
		t.Errorf("transfer create during repair err = %v, want conflict", err)
	}
	if _, err := scrapSvc.Create(&dto.CreateScrapReq{DeviceID: d.ID, Reason: "老化"}, "dept01"); !isConflict(err) {
		t.Errorf("scrap create during repair err = %v, want conflict", err)
	}

	// 维修前已存在的调拨申请，维修中审批也应被挡住。
	if _, err := transferSvc.Approve(tr.ID, &dto.TransferApproveReq{}, "dean01"); !isConflict(err) {
		t.Errorf("transfer approve during repair err = %v, want conflict", err)
	}
}

func TestRepairCompleteResumedRestoresPreviousStatus(t *testing.T) {
	env, maintSvc, _, _, deviceSvc := newRepairFlowEnv(t)
	d := createTestDevice(t, deviceSvc, "RP-002")

	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "eng")
	if err != nil {
		t.Fatalf("create repair failed: %v", err)
	}
	if _, err := maintSvc.Start(m.ID, &dto.StartMaintenanceReq{Engineer: "王工"}, "eng"); err != nil {
		t.Fatalf("start failed: %v", err)
	}

	// 完工结论缺失应返回 400。
	if _, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{Content: "已维修"}, "eng"); !isBadRequest(err) {
		t.Errorf("complete without outcome err = %v, want 400", err)
	}

	done, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{
		Content: "更换电源板", Cost: 300, RepairOutcome: constants.RepairOutcomeResumed,
	}, "eng")
	if err != nil {
		t.Fatalf("complete resumed failed: %v", err)
	}
	if done.RepairOutcome != constants.RepairOutcomeResumed {
		t.Errorf("repair_outcome = %q", done.RepairOutcome)
	}
	// 确认完工结论已持久化。
	stored, err := repository.NewMaintenanceRepository(env.db).FindByID(done.ID)
	if err != nil {
		t.Fatalf("reload completed record failed: %v", err)
	}
	if stored.RepairOutcome != constants.RepairOutcomeResumed {
		t.Errorf("persisted repair_outcome = %q, want resumed", stored.RepairOutcome)
	}
	got, _ := deviceSvc.repo.FindByID(d.ID)
	if got.Status != constants.DeviceStatusInUse {
		t.Errorf("device status = %q, want restored in_use", got.Status)
	}

	// 已完工的工单不能再次完工。
	if _, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{
		Content: "x", RepairOutcome: constants.RepairOutcomeResumed,
	}, "eng"); !isConflict(err) {
		t.Errorf("double complete err = %v, want conflict", err)
	}
}

func TestRepairCompleteBrokenScrapsDevice(t *testing.T) {
	_, maintSvc, _, _, deviceSvc := newRepairFlowEnv(t)
	d := createTestDevice(t, deviceSvc, "RP-003")

	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "eng")
	if err != nil {
		t.Fatalf("create repair failed: %v", err)
	}
	if _, err := maintSvc.Start(m.ID, &dto.StartMaintenanceReq{Engineer: "王工"}, "eng"); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	done, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{
		Content: "主板烧毁", RepairOutcome: constants.RepairOutcomeBroken,
	}, "eng")
	if err != nil {
		t.Fatalf("complete broken failed: %v", err)
	}
	if done.RepairOutcome != constants.RepairOutcomeBroken {
		t.Errorf("repair_outcome = %q", done.RepairOutcome)
	}
	got, _ := deviceSvc.repo.FindByID(d.ID)
	if got.Status != constants.DeviceStatusScrapped {
		t.Errorf("device status = %q, want scrapped", got.Status)
	}
}

func TestRepairCancelRestoresPreviousStatus(t *testing.T) {
	_, maintSvc, _, _, deviceSvc := newRepairFlowEnv(t)
	d := createTestDevice(t, deviceSvc, "RP-004")

	// 待处理状态直接取消也应恢复。
	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "eng")
	if err != nil {
		t.Fatalf("create repair failed: %v", err)
	}
	if _, err := maintSvc.Cancel(m.ID, &dto.CancelMaintenanceReq{Reason: "误报"}, "eng"); err != nil {
		t.Fatalf("cancel pending failed: %v", err)
	}
	got, _ := deviceSvc.repo.FindByID(d.ID)
	if got.Status != constants.DeviceStatusInUse {
		t.Errorf("device status after cancel = %q, want in_use", got.Status)
	}

	// 处理中取消同样恢复。
	m2, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "eng")
	if err != nil {
		t.Fatalf("create repair 2 failed: %v", err)
	}
	if _, err := maintSvc.Start(m2.ID, &dto.StartMaintenanceReq{Engineer: "王工"}, "eng"); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if _, err := maintSvc.Cancel(m2.ID, &dto.CancelMaintenanceReq{Reason: "无需维修"}, "eng"); err != nil {
		t.Fatalf("cancel in_progress failed: %v", err)
	}
	got2, _ := deviceSvc.repo.FindByID(d.ID)
	if got2.Status != constants.DeviceStatusInUse {
		t.Errorf("device status after cancel in_progress = %q, want in_use", got2.Status)
	}

	// 已取消的工单不能重复取消。
	if _, err := maintSvc.Cancel(m2.ID, &dto.CancelMaintenanceReq{}, "eng"); !isConflict(err) {
		t.Errorf("double cancel err = %v, want conflict", err)
	}
}

func TestNonRepairMaintenanceDoesNotTouchDeviceStatus(t *testing.T) {
	_, maintSvc, _, _, deviceSvc := newRepairFlowEnv(t)
	d := createTestDevice(t, deviceSvc, "RP-005")

	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeMonthly}, "eng")
	if err != nil {
		t.Fatalf("create monthly plan failed: %v", err)
	}
	if m.PreviousStatus != "" {
		t.Errorf("previous_status = %q, want empty for non-repair", m.PreviousStatus)
	}
	got, _ := deviceSvc.repo.FindByID(d.ID)
	if got.Status != constants.DeviceStatusInUse {
		t.Errorf("device status = %q, want unchanged in_use", got.Status)
	}
	if _, err := maintSvc.Start(m.ID, &dto.StartMaintenanceReq{Engineer: "王工"}, "eng"); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if _, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{Content: "例行保养"}, "eng"); err != nil {
		t.Fatalf("complete non-repair failed: %v", err)
	}
	got2, _ := deviceSvc.repo.FindByID(d.ID)
	if got2.Status != constants.DeviceStatusInUse {
		t.Errorf("device status after non-repair complete = %q, want in_use", got2.Status)
	}
}

func TestScrapApproveBlockedDuringRepair(t *testing.T) {
	_, maintSvc, _, scrapSvc, deviceSvc := newRepairFlowEnv(t)
	d := createTestDevice(t, deviceSvc, "RP-006")

	sr, err := scrapSvc.Create(&dto.CreateScrapReq{DeviceID: d.ID, Reason: "老化严重"}, "dept01")
	if err != nil {
		t.Fatalf("create scrap failed: %v", err)
	}
	if _, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "eng"); err != nil {
		t.Fatalf("create repair failed: %v", err)
	}
	// 维修中审批维修前提交的报废申请必须被挡住。
	if _, err := scrapSvc.Approve(sr.ID, &dto.ScrapApproveReq{}, "dean01"); !isConflict(err) {
		t.Errorf("scrap approve during repair err = %v, want conflict", err)
	}
	got, _ := deviceSvc.repo.FindByID(d.ID)
	if got.Status != constants.DeviceStatusUnderMaintenance {
		t.Errorf("device status = %q, want still under_maintenance", got.Status)
	}
}

func TestTransferApprovedRejectedWhenRepairCompletedAfterSubmission(t *testing.T) {
	_, maintSvc, transferSvc, _, deviceSvc := newRepairFlowEnv(t)
	d := createTestDevice(t, deviceSvc, "RP-007")

	// 先发起调拨申请。
	tr, err := transferSvc.Create(&dto.CreateTransferReq{DeviceID: d.ID, ToDepartment: "急诊科"}, "dept01")
	if err != nil {
		t.Fatalf("create transfer failed: %v", err)
	}

	// 申请之后设备经历完整故障维修并完工（恢复使用）。
	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "eng")
	if err != nil {
		t.Fatalf("create repair failed: %v", err)
	}
	if _, err := maintSvc.Start(m.ID, &dto.StartMaintenanceReq{Engineer: "王工"}, "eng"); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	if _, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{
		Content: "修复", RepairOutcome: constants.RepairOutcomeResumed,
	}, "eng"); err != nil {
		t.Fatalf("complete failed: %v", err)
	}

	// 设备虽已恢复使用中，但申请提交后发生过维修，审批必须被挡住并提示刷新。
	if _, err := transferSvc.Approve(tr.ID, &dto.TransferApproveReq{}, "dean01"); !isConflict(err) {
		t.Fatalf("approve after repair err = %v, want conflict", err)
	}
	// 申请仍是待审批，但业务上需要驳回/重新发起；设备状态保持恢复后的原状态。
	got, _ := deviceSvc.repo.FindByID(d.ID)
	if got.Status != constants.DeviceStatusInUse {
		t.Errorf("device status = %q, want in_use", got.Status)
	}
}

func TestTransferApprovedAllowedWhenRepairCancelledAfterSubmission(t *testing.T) {
	_, maintSvc, transferSvc, _, deviceSvc := newRepairFlowEnv(t)
	d := createTestDevice(t, deviceSvc, "RP-008")

	tr, err := transferSvc.Create(&dto.CreateTransferReq{DeviceID: d.ID, ToDepartment: "急诊科"}, "dept01")
	if err != nil {
		t.Fatalf("create transfer failed: %v", err)
	}
	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "eng")
	if err != nil {
		t.Fatalf("create repair failed: %v", err)
	}
	// 维修取消后设备恢复原状态，既有申请仍然有效。
	if _, err := maintSvc.Cancel(m.ID, &dto.CancelMaintenanceReq{Reason: "误报"}, "eng"); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if _, err := transferSvc.Approve(tr.ID, &dto.TransferApproveReq{}, "dean01"); err != nil {
		t.Fatalf("approve after cancelled repair should succeed, got: %v", err)
	}
	got, _ := deviceSvc.repo.FindByID(d.ID)
	if got.Department != "急诊科" {
		t.Errorf("department = %q, want 急诊科", got.Department)
	}
}

func isConflict(err error) bool {
	appErr, ok := err.(*util.AppError)
	return ok && appErr.Code == http.StatusConflict
}

func isBadRequest(err error) bool {
	appErr, ok := err.(*util.AppError)
	return ok && appErr.Code == http.StatusBadRequest
}
