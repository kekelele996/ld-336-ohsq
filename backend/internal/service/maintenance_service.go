package service

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/dto"
	"github.com/medasset/medasset/internal/model"
	"github.com/medasset/medasset/internal/repository"
	"github.com/medasset/medasset/internal/util"
	"github.com/medasset/medasset/pkg/pointerx"
	"gorm.io/gorm"
)

// MaintenanceService 维护保养与故障维修服务。
type MaintenanceService struct {
	repo   *repository.MaintenanceRepository
	device *repository.DeviceRepository
	audit  *AuditService
	log    *slog.Logger
}

func NewMaintenanceService(repo *repository.MaintenanceRepository, device *repository.DeviceRepository, audit *AuditService, log *slog.Logger) *MaintenanceService {
	return &MaintenanceService{repo: repo, device: device, audit: audit, log: log}
}

// Create 创建保养/维修工单（报修或计划执行）。
// 故障维修单创建时在同一事务内记住设备原状态并将设备转为“维修中”；
// 已报废或维修中的设备不允许再创建故障维修单。
func (s *MaintenanceService) Create(req *dto.CreateMaintenanceReq, operator string) (*model.MaintenanceRecord, error) {
	var created *model.MaintenanceRecord
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		d, err := s.device.FindByIDForUpdate(tx, req.DeviceID)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(req.DeviceID), nil)
		}
		if err != nil {
			return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
		}
		if req.Type == constants.MaintenanceTypeRepair {
			// 维修中/已报废设备不允许再创建故障维修单。
			if err := rejectIfDeviceBusy(d, "create_maintenance", operator, s.log.Info); err != nil {
				return err
			}
			active, err := s.repo.HasActiveRepairTx(tx, d.ID)
			if err != nil {
				return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
			}
			if active {
				return util.NewAppError(http.StatusConflict, constants.MsgDeviceInMaintenance, nil)
			}
		} else if d.Status == constants.DeviceStatusScrapped {
			// 保养类工单也不允许建在已报废设备上。
			return util.NewAppError(http.StatusConflict, constants.MsgDeviceInScrapped, nil)
		}
		m := &model.MaintenanceRecord{
			RecordNo:         util.GenSerial("MT"),
			DeviceID:         d.ID,
			DeviceName:       d.Name,
			Type:             req.Type,
			Status:           constants.MaintenanceStatusPending,
			PlannedDate:      req.PlannedDate,
			Content:          req.Content,
			FaultDescription: req.FaultDescription,
			Engineer:         req.Engineer,
			CreatedBy:        operator,
		}
		// 故障维修：创建即记住原状态并转为维修中。
		if req.Type == constants.MaintenanceTypeRepair {
			m.PreviousStatus = d.Status
			if err := s.device.UpdateStatusTx(tx, d.ID, constants.DeviceStatusUnderMaintenance); err != nil {
				return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
			}
			s.log.Info(fmt.Sprintf(constants.LogDeviceStatusChanged, d.ID, d.Status, constants.DeviceStatusUnderMaintenance))
		}
		if err := tx.Create(m).Error; err != nil {
			return util.NewAppError(http.StatusInternalServerError, "创建工单失败: device_name="+d.Name, err)
		}
		created = m
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogMaintenanceCreated, created.RecordNo, created.DeviceID, created.Type, created.Status))
	s.audit.Record(0, operator, "CREATE", "maintenance", util.Uint64String(created.ID), "创建保养/维修工单: "+created.RecordNo, operator, "")
	return created, nil
}

// GeneratePlans 根据设备类型自动生成保养计划（日检/周检/月检/年检，无待处理计划时生成）。
func (s *MaintenanceService) GeneratePlans(operator string) (int, error) {
	devices, _, err := s.device.List(1, 200, "", "", "", "")
	if err != nil {
		return 0, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	created := 0
	types := []string{constants.MaintenanceTypeDaily, constants.MaintenanceTypeWeekly, constants.MaintenanceTypeMonthly, constants.MaintenanceTypeYearly}
	for _, d := range devices {
		if d.Status == constants.DeviceStatusScrapped {
			continue
		}
		_ = s.repo.DB().Transaction(func(tx *gorm.DB) error {
			for _, t := range types {
				exists, err := s.existsPending(tx, d.ID, t)
				if err != nil {
					return err
				}
				if exists {
					continue
				}
				now := time.Now()
				m := &model.MaintenanceRecord{
					RecordNo:    util.GenSerial("MT"),
					DeviceID:    d.ID,
					DeviceName:  d.Name,
					Type:        t,
					Status:      constants.MaintenanceStatusPending,
					PlannedDate: planDate(now, t),
					Content:     "自动生成" + util.MaintenanceTypeText(t) + "保养计划",
					CreatedBy:   operator,
				}
				if err := tx.Create(m).Error; err != nil {
					return err
				}
				created++
			}
			return nil
		})
	}
	s.log.Info("自动生成保养计划完成", "created", created, "operator", operator)
	return created, nil
}

func (s *MaintenanceService) existsPending(tx *gorm.DB, deviceID uint, mType string) (bool, error) {
	var n int64
	err := tx.Model(&model.MaintenanceRecord{}).
		Where("device_id = ? AND type = ? AND status = ?", deviceID, mType, constants.MaintenanceStatusPending).
		Count(&n).Error
	return n > 0, err
}

// List 分页查询保养/维修记录。
func (s *MaintenanceService) List(page, pageSize int, deviceID uint, mType, status string) (*util.PageResult, error) {
	list, total, err := s.repo.List(page, pageSize, deviceID, mType, status)
	if err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	return &util.PageResult{List: list, Total: total, Page: page, PageSize: pageSize}, nil
}

// Start 开始执行工单。故障维修单创建时设备已进入维修中，这里仅更新工单执行信息。
func (s *MaintenanceService) Start(id uint, req *dto.StartMaintenanceReq, operator string) (*model.MaintenanceRecord, error) {
	var updated *model.MaintenanceRecord
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		m, err := s.repo.FindByIDForUpdate(tx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "工单不存在: id="+util.Uint64String(id), nil)
		}
		if err != nil {
			return err
		}
		if m.Status != constants.MaintenanceStatusPending {
			return util.NewAppError(http.StatusConflict, constants.MsgInvalidStatus, nil)
		}
		m.Status = constants.MaintenanceStatusInProgress
		if req.Engineer != "" {
			m.Engineer = req.Engineer
		}
		if err := s.repo.UpdateTx(tx, m); err != nil {
			return err
		}
		updated = m
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogMaintenanceStarted, updated.RecordNo, updated.Engineer, updated.Status))
	s.audit.Record(0, operator, "START", "maintenance", util.Uint64String(updated.ID), "开始执行: "+updated.RecordNo, operator, "")
	return updated, nil
}

// Complete 完成工单（更新工时/成本/配件）。
// 故障维修完工必须给出结论：resumed 恢复创建时记住的原状态；broken 设备转已报废。
// 维修类工单按“设备行 → 工单行”顺序加 FOR UPDATE 锁（与调拨/报废审批一致），
// 保证两边并发提交时只可能一边成功，后到一方看到最新设备状态并收到提示。
func (s *MaintenanceService) Complete(id uint, req *dto.CompleteMaintenanceReq, operator string) (*model.MaintenanceRecord, error) {
	var updated *model.MaintenanceRecord
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		// 先非锁定预读，拿到设备 ID 等基础信息。
		pre, err := s.repo.FindByIDTx(tx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "工单不存在: id="+util.Uint64String(id), nil)
		}
		if err != nil {
			return err
		}
		var d *model.Device
		if pre.Type == constants.MaintenanceTypeRepair {
			// 固定锁顺序：先锁设备行。
			d, err = s.device.FindByIDForUpdate(tx, pre.DeviceID)
			if errors.Is(err, repository.ErrNotFound) {
				return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(pre.DeviceID), nil)
			}
			if err != nil {
				return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
			}
		}
		// 再锁工单行，读取权威最新状态。
		m, err := s.repo.FindByIDForUpdate(tx, id)
		if err != nil {
			return err
		}
		if m.Status != constants.MaintenanceStatusInProgress && m.Status != constants.MaintenanceStatusPending {
			return util.NewAppError(http.StatusConflict, constants.MsgInvalidStatus, nil)
		}
		if m.Type == constants.MaintenanceTypeRepair &&
			req.RepairOutcome != constants.RepairOutcomeResumed &&
			req.RepairOutcome != constants.RepairOutcomeBroken {
			return util.NewAppError(http.StatusBadRequest, constants.MsgRepairOutcomeRequired, nil)
		}
		now := time.Now()
		m.Status = constants.MaintenanceStatusCompleted
		m.ExecutedDate = &now
		m.Content = req.Content
		m.ReplacedParts = req.ReplacedParts
		m.WorkHours = req.WorkHours
		m.Cost = req.Cost
		m.RepairResult = req.RepairResult
		if m.Type == constants.MaintenanceTypeRepair {
			m.RepairOutcome = req.RepairOutcome
		}
		if err := s.repo.UpdateTx(tx, m); err != nil {
			return err
		}
		if m.Type == constants.MaintenanceTypeRepair {
			if req.RepairOutcome == constants.RepairOutcomeBroken {
				// 无法修好：设备转已报废。
				if err := s.device.UpdateStatusTx(tx, d.ID, constants.DeviceStatusScrapped); err != nil {
					return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
				}
				s.log.Info(fmt.Sprintf(constants.LogMaintenanceDeviceScrapped, d.ID, m.RecordNo))
			} else {
				// 已修复继续使用：恢复创建时记住的原状态。
				// 设备已不是维修中说明状态被其他流程改动，后到一方按最新状态提示。
				if err := ensureDeviceCurrentStatus(d, constants.DeviceStatusUnderMaintenance); err != nil {
					return err
				}
				target := restoredDeviceStatus(m.PreviousStatus)
				if err := s.device.UpdateStatusTx(tx, d.ID, target); err != nil {
					return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
				}
				s.log.Info(fmt.Sprintf(constants.LogMaintenanceDeviceRestored, d.ID, constants.DeviceStatusUnderMaintenance, target, req.RepairOutcome))
			}
		}
		if err := tx.Model(&model.Device{}).Where("id = ?", m.DeviceID).Update("last_maintenance_at", now).Error; err != nil {
			return err
		}
		updated = m
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogMaintenanceCompleted, updated.RecordNo, updated.DeviceID, updated.Cost, updated.Status))
	s.audit.Record(0, operator, "COMPLETE", "maintenance", util.Uint64String(updated.ID), "完成工单: "+updated.RecordNo, operator, "")
	return updated, nil
}

// Cancel 取消工单。故障维修单取消时设备恢复创建时记住的原状态。
// 取消同时允许待处理与处理中（维修中）的工单。
// 维修类工单同样按“设备行 → 工单行”顺序加锁，与审批流程互斥。
func (s *MaintenanceService) Cancel(id uint, req *dto.CancelMaintenanceReq, operator string) (*model.MaintenanceRecord, error) {
	var updated *model.MaintenanceRecord
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		// 非锁定预读以确定工单类型与设备 ID。
		pre, err := s.repo.FindByIDTx(tx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "工单不存在: id="+util.Uint64String(id), nil)
		}
		if err != nil {
			return err
		}
		var d *model.Device
		if pre.Type == constants.MaintenanceTypeRepair {
			d, err = s.device.FindByIDForUpdate(tx, pre.DeviceID)
			if errors.Is(err, repository.ErrNotFound) {
				return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(pre.DeviceID), nil)
			}
			if err != nil {
				return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
			}
		}
		m, err := s.repo.FindByIDForUpdate(tx, id)
		if err != nil {
			return err
		}
		if m.Status != constants.MaintenanceStatusPending && m.Status != constants.MaintenanceStatusInProgress {
			return util.NewAppError(http.StatusConflict, constants.MsgInvalidStatus, nil)
		}
		if m.Type == constants.MaintenanceTypeRepair {
			// 设备已不是维修中说明状态被其他流程改动，后到的取消看到最新状态并收到提示。
			if err := ensureDeviceCurrentStatus(d, constants.DeviceStatusUnderMaintenance); err != nil {
				return err
			}
			target := restoredDeviceStatus(m.PreviousStatus)
			if err := s.device.UpdateStatusTx(tx, d.ID, target); err != nil {
				return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
			}
			s.log.Info(fmt.Sprintf(constants.LogMaintenanceDeviceRestored, d.ID, constants.DeviceStatusUnderMaintenance, target, "cancelled"))
		}
		m.Status = constants.MaintenanceStatusCancelled
		if req.Reason != "" {
			m.RepairResult = "取消原因: " + req.Reason
		}
		if err := s.repo.UpdateTx(tx, m); err != nil {
			return err
		}
		updated = m
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogMaintenanceCancelled, updated.RecordNo, req.Reason, updated.Status))
	s.audit.Record(0, operator, "CANCEL", "maintenance", util.Uint64String(updated.ID), "取消工单: "+updated.RecordNo, operator, "")
	return updated, nil
}

func planDate(now time.Time, mType string) *time.Time {
	var add time.Duration
	switch mType {
	case constants.MaintenanceTypeDaily:
		add = 24 * time.Hour
	case constants.MaintenanceTypeWeekly:
		add = 7 * 24 * time.Hour
	case constants.MaintenanceTypeMonthly:
		add = 30 * 24 * time.Hour
	case constants.MaintenanceTypeYearly:
		add = 365 * 24 * time.Hour
	default:
		add = 30 * 24 * time.Hour
	}
	t := now.Add(add)
	return pointerx.TimePtr(t)
}
