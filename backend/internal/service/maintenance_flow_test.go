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

func newDevice(t *testing.T, env *testEnv, status string) *model.Device {
	t.Helper()
	d := &model.Device{
		AssetCode:  util.GenSerial("ASSET"),
		Name:       "监护仪",
		Category:   "生命支持",
		Department: "ICU",
		Status:     status,
	}
	if err := env.db.Create(d).Error; err != nil {
		t.Fatalf("create device failed: %v", err)
	}
	return d
}

func appStatusCode(t *testing.T, err error) int {
	t.Helper()
	appErr, ok := err.(*util.AppError)
	if !ok {
		t.Fatalf("expected AppError, got %T: %v", err, err)
	}
	return appErr.Code
}

func deviceStatus(t *testing.T, env *testEnv, id uint) string {
	t.Helper()
	var d model.Device
	if err := env.db.First(&d, id).Error; err != nil {
		t.Fatalf("reload device failed: %v", err)
	}
	return d.Status
}

// 维修单创建即转维修中并记住原状态；取消后恢复原状态。
func TestRepairCreateThenCancelRestoresOriginalStatus(t *testing.T) {
	env := newTestServiceEnv(t)
	maintSvc := NewMaintenanceService(
		repository.NewMaintenanceRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)

	for _, original := range []string{constants.DeviceStatusInStorage, constants.DeviceStatusInUse, constants.DeviceStatusDisabled} {
		d := newDevice(t, env, original)

		m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair,
			FaultDescription: "开不了机"}, "engineer1")
		if err != nil {
			t.Fatalf("Create repair failed: %v", err)
		}
		if m.OriginalStatus != original {
			t.Errorf("original_status = %q, want %q", m.OriginalStatus, original)
		}
		if got := deviceStatus(t, env, d.ID); got != constants.DeviceStatusUnderMaintenance {
			t.Fatalf("after create device status = %q, want under_maintenance", got)
		}

		// 待处理状态可直接取消，设备恢复原状态。
		if _, err := maintSvc.Cancel(m.ID, &dto.CancelMaintenanceReq{Reason: "误报"}, "engineer1"); err != nil {
			t.Fatalf("Cancel pending failed: %v", err)
		}
		if got := deviceStatus(t, env, d.ID); got != original {
			t.Errorf("after cancel device status = %q, want original %q", got, original)
		}
	}
}

// 执行中取消也必须恢复原状态。
func TestRepairCancelInProgressRestoresOriginalStatus(t *testing.T) {
	env := newTestServiceEnv(t)
	maintSvc := NewMaintenanceService(
		repository.NewMaintenanceRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)
	d := newDevice(t, env, constants.DeviceStatusInUse)

	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "engineer1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if _, err := maintSvc.Start(m.ID, &dto.StartMaintenanceReq{Engineer: "张工"}, "engineer1"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if _, err := maintSvc.Cancel(m.ID, &dto.CancelMaintenanceReq{Reason: "配件缺货"}, "engineer1"); err != nil {
		t.Fatalf("Cancel in_progress failed: %v", err)
	}
	if got := deviceStatus(t, env, d.ID); got != constants.DeviceStatusInUse {
		t.Errorf("after cancel in_progress device status = %q, want in_use", got)
	}
}

// 完工选择继续使用 → 恢复原状态（不能无脑变成使用中）。
func TestRepairCompleteContinueUseRestoresOriginalStatus(t *testing.T) {
	env := newTestServiceEnv(t)
	maintSvc := NewMaintenanceService(
		repository.NewMaintenanceRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)
	d := newDevice(t, env, constants.DeviceStatusInStorage)

	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "engineer1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if _, err := maintSvc.Start(m.ID, &dto.StartMaintenanceReq{Engineer: "张工"}, "engineer1"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// 缺完工结论必须报错。
	if _, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{Content: "更换电源板"}, "engineer1"); err == nil {
		t.Fatal("expected error when repair_outcome missing")
	} else if appStatusCode(t, err) != http.StatusBadRequest {
		t.Fatalf("status code = %d, want 400", appStatusCode(t, err))
	}

	completed, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{
		Content: "更换电源板", RepairOutcome: constants.RepairOutcomeContinueUse}, "engineer1")
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if completed.RepairOutcome != constants.RepairOutcomeContinueUse {
		t.Errorf("repair_outcome = %q", completed.RepairOutcome)
	}
	if got := deviceStatus(t, env, d.ID); got != constants.DeviceStatusInStorage {
		t.Errorf("after complete continue_use device status = %q, want in_storage", got)
	}

	// 已完工不允许再次取消。
	if _, err := maintSvc.Cancel(m.ID, &dto.CancelMaintenanceReq{}, "engineer1"); err == nil {
		t.Fatal("expected error cancelling completed record")
	}
}

// 完工选择无法修好 → 设备转已报废。
func TestRepairCompleteUnrepairableScrapsDevice(t *testing.T) {
	env := newTestServiceEnv(t)
	maintSvc := NewMaintenanceService(
		repository.NewMaintenanceRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)
	d := newDevice(t, env, constants.DeviceStatusInUse)

	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "engineer1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if _, err := maintSvc.Start(m.ID, &dto.StartMaintenanceReq{Engineer: "张工"}, "engineer1"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if _, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{
		Content: "主板烧毁", RepairOutcome: constants.RepairOutcomeUnrepairable}, "engineer1"); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got := deviceStatus(t, env, d.ID); got != constants.DeviceStatusScrapped {
		t.Errorf("after unrepairable device status = %q, want scrapped", got)
	}
}

// 维修中的设备，新建调拨/报废申请都要被挡住。
func TestCreateTransferAndScrapBlockedUnderMaintenance(t *testing.T) {
	env := newTestServiceEnv(t)
	maintSvc := NewMaintenanceService(
		repository.NewMaintenanceRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)
	transferSvc := NewTransferService(
		repository.NewTransferRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)
	scrapSvc := NewScrapService(
		repository.NewScrapRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)

	d := newDevice(t, env, constants.DeviceStatusInUse)
	if _, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "engineer1"); err != nil {
		t.Fatalf("Create repair failed: %v", err)
	}

	if _, err := transferSvc.Create(&dto.CreateTransferReq{DeviceID: d.ID, ToDepartment: "放射科"}, "dept1"); err == nil {
		t.Error("expected transfer create blocked under maintenance")
	} else if appStatusCode(t, err) != http.StatusConflict {
		t.Errorf("transfer create code = %d, want 409", appStatusCode(t, err))
	}
	if _, err := scrapSvc.Create(&dto.CreateScrapReq{DeviceID: d.ID, Reason: "老化"}, "dept1"); err == nil {
		t.Error("expected scrap create blocked under maintenance")
	} else if appStatusCode(t, err) != http.StatusConflict {
		t.Errorf("scrap create code = %d, want 409", appStatusCode(t, err))
	}

	// 重复创建故障维修单也要挡住。
	if _, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "engineer1"); err == nil {
		t.Error("expected duplicate repair create blocked")
	}
}

// 审批时设备若已进入维修中，调拨/报废审批必须失败并返回带最新状态的提示。
func TestApproveBlockedWhenDeviceEnteredMaintenanceAfterRequest(t *testing.T) {
	env := newTestServiceEnv(t)
	maintSvc := NewMaintenanceService(
		repository.NewMaintenanceRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)
	transferSvc := NewTransferService(
		repository.NewTransferRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)
	scrapSvc := NewScrapService(
		repository.NewScrapRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)

	// 场景一：调拨申请先存在，之后设备进维修。
	d1 := newDevice(t, env, constants.DeviceStatusInUse)
	tr, err := transferSvc.Create(&dto.CreateTransferReq{DeviceID: d1.ID, ToDepartment: "放射科"}, "dept1")
	if err != nil {
		t.Fatalf("transfer Create failed: %v", err)
	}
	if _, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d1.ID, Type: constants.MaintenanceTypeRepair}, "engineer1"); err != nil {
		t.Fatalf("repair Create failed: %v", err)
	}
	if _, err := transferSvc.Approve(tr.ID, &dto.TransferApproveReq{Comment: "同意"}, "dean1"); err == nil {
		t.Fatal("expected transfer approve blocked, got success")
	} else if appStatusCode(t, err) != http.StatusConflict {
		t.Fatalf("transfer approve code = %d, want 409", appStatusCode(t, err))
	}
	// 调拨仍是待审批，设备仍是维修中：只有维修流程能推进。
	var tr2 model.TransferRequest
	if err := env.db.First(&tr2, tr.ID).Error; err != nil {
		t.Fatalf("reload transfer failed: %v", err)
	}
	if tr2.Status != constants.TransferStatusPending {
		t.Errorf("transfer status = %q, want pending", tr2.Status)
	}

	// 场景二：报废申请先存在，之后设备进维修。
	d2 := newDevice(t, env, constants.DeviceStatusInUse)
	sr, err := scrapSvc.Create(&dto.CreateScrapReq{DeviceID: d2.ID, Reason: "老化"}, "dept1")
	if err != nil {
		t.Fatalf("scrap Create failed: %v", err)
	}
	if _, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d2.ID, Type: constants.MaintenanceTypeRepair}, "engineer1"); err != nil {
		t.Fatalf("repair Create failed: %v", err)
	}
	if _, err := scrapSvc.Approve(sr.ID, &dto.ScrapApproveReq{Comment: "同意"}, "dean1"); err == nil {
		t.Fatal("expected scrap approve blocked, got success")
	} else if appStatusCode(t, err) != http.StatusConflict {
		t.Fatalf("scrap approve code = %d, want 409", appStatusCode(t, err))
	}
	if got := deviceStatus(t, env, d2.ID); got != constants.DeviceStatusUnderMaintenance {
		t.Errorf("device status = %q, want under_maintenance", got)
	}
}

// 维修完工与调拨审批“前后脚”提交：先到的调拨审批成功后，完工必须看到设备已不在维修中并失败，
// 两边不允许都成功（SQLite 不行锁，这里按状态机顺序模拟先后两笔事务的可见结果）。
func TestCompleteBlockedAfterTransferApproved(t *testing.T) {
	env := newTestServiceEnv(t)
	maintSvc := NewMaintenanceService(
		repository.NewMaintenanceRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)

	d := newDevice(t, env, constants.DeviceStatusInUse)
	// 维修单与设备进维修中先发生，但调拨申请在维修之前已提交（审批在维修期间本应被拦截；
	// 若审批先抢到设备行锁而成功，设备科室已变更且状态离开维修中——此处直接构造“审批先到”的终态）。
	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "engineer1")
	if err != nil {
		t.Fatalf("repair Create failed: %v", err)
	}
	if _, err := maintSvc.Start(m.ID, &dto.StartMaintenanceReq{Engineer: "张工"}, "engineer1"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	// 模拟调拨审批先提交成功：设备在维修开始前一刻状态还是 in_use（即审批先拿到锁），
	// 审批后设备仍非维修中。直接将设备置为 in_use 表示另一笔事务已先行提交。
	if err := env.db.Model(&model.Device{}).Where("id = ?", d.ID).Update("status", constants.DeviceStatusInUse).Error; err != nil {
		t.Fatalf("simulate status change failed: %v", err)
	}
	if _, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{
		Content: "维修完成", RepairOutcome: constants.RepairOutcomeContinueUse}, "engineer1"); err == nil {
		t.Fatal("expected complete blocked when device left maintenance, got success")
	} else if appStatusCode(t, err) != http.StatusConflict {
		t.Fatalf("complete code = %d, want 409", appStatusCode(t, err))
	}
}

// 无法修好完工后，待审批的调拨再批准必须被挡住（设备已报废）。
func TestTransferApproveBlockedAfterUnrepairableComplete(t *testing.T) {
	env := newTestServiceEnv(t)
	maintSvc := NewMaintenanceService(
		repository.NewMaintenanceRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)
	transferSvc := NewTransferService(
		repository.NewTransferRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)

	d := newDevice(t, env, constants.DeviceStatusInUse)
	// 先在设备正常时提交调拨申请。
	tr, err := transferSvc.Create(&dto.CreateTransferReq{DeviceID: d.ID, ToDepartment: "放射科"}, "dept1")
	if err != nil {
		t.Fatalf("transfer Create failed: %v", err)
	}
	// 随后故障维修走完并判定无法修好。
	m, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "engineer1")
	if err != nil {
		t.Fatalf("repair Create failed: %v", err)
	}
	if _, err := maintSvc.Start(m.ID, &dto.StartMaintenanceReq{Engineer: "张工"}, "engineer1"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if _, err := maintSvc.Complete(m.ID, &dto.CompleteMaintenanceReq{
		Content: "无法修复", RepairOutcome: constants.RepairOutcomeUnrepairable}, "engineer1"); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	// 后到的调拨审批必须失败。
	if _, err := transferSvc.Approve(tr.ID, &dto.TransferApproveReq{}, "dean1"); err == nil {
		t.Fatal("expected transfer approve blocked after scrapped, got success")
	} else if appStatusCode(t, err) != http.StatusConflict {
		t.Fatalf("code = %d, want 409", appStatusCode(t, err))
	}
	if got := deviceStatus(t, env, d.ID); got != constants.DeviceStatusScrapped {
		t.Errorf("device status = %q, want scrapped", got)
	}
}

// 已报废设备不能创建故障维修单。
func TestRepairCreateBlockedForScrappedDevice(t *testing.T) {
	env := newTestServiceEnv(t)
	maintSvc := NewMaintenanceService(
		repository.NewMaintenanceRepository(env.db), repository.NewDeviceRepository(env.db), env.audit, env.logger)
	d := newDevice(t, env, constants.DeviceStatusScrapped)
	if _, err := maintSvc.Create(&dto.CreateMaintenanceReq{DeviceID: d.ID, Type: constants.MaintenanceTypeRepair}, "engineer1"); err == nil {
		t.Fatal("expected repair create blocked for scrapped device")
	}
}

// 并发哨兵：等待行锁期间设备状态被维修流程改动时，后到一方必须被拒并看到新旧状态。
func TestRejectIfStatusChangedSinceSnapshot(t *testing.T) {
	before := &model.Device{Name: "除颤仪", Status: constants.DeviceStatusUnderMaintenance}
	latest := &model.Device{Name: "除颤仪", Status: constants.DeviceStatusInUse}

	if err := rejectIfStatusChangedSinceSnapshot(before, latest, "调拨审批"); err == nil {
		t.Fatal("expected conflict when status changed between snapshot and lock")
	} else if appStatusCode(t, err) != http.StatusConflict {
		t.Fatalf("code = %d, want 409", appStatusCode(t, err))
	}

	same := &model.Device{Name: "除颤仪", Status: constants.DeviceStatusUnderMaintenance}
	if err := rejectIfStatusChangedSinceSnapshot(before, same, "调拨审批"); err != nil {
		t.Fatalf("expected no error when status unchanged, got %v", err)
	}
}
