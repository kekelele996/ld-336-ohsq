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
// 故障维修工单创建时即记住设备原状态并将设备转为维修中，全程在同一事务内对设备行加锁，
// 防止与调拨/报废审批并发导致两边都成功。
func (s *MaintenanceService) Create(req *dto.CreateMaintenanceReq, operator string) (*model.MaintenanceRecord, error) {
	m := &model.MaintenanceRecord{}
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		d, err := s.device.FindByIDForUpdate(tx, req.DeviceID)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(req.DeviceID), nil)
		}
		if err != nil {
			return err
		}
		if d.Status == constants.DeviceStatusScrapped {
			return util.NewAppError(http.StatusConflict, constants.MsgDeviceInScrapped, nil)
		}
		if req.Type == constants.MaintenanceTypeRepair {
			if d.Status == constants.DeviceStatusUnderMaintenance {
				return util.NewAppError(http.StatusConflict, constants.MsgDeviceUnderMaintenance, nil)
			}
		}
		*m = model.MaintenanceRecord{
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
		// 故障维修：创建即记住原状态并转维修中，取消/完工时据此恢复，避免状态被改错。
		if req.Type == constants.MaintenanceTypeRepair {
			m.OriginalStatus = d.Status
			if err := s.device.UpdateStatusTx(tx, d.ID, constants.DeviceStatusUnderMaintenance); err != nil {
				return err
			}
		}
		if err := tx.Create(m).Error; err != nil {
			return util.NewAppError(http.StatusInternalServerError, "创建工单失败: device_name="+d.Name, err)
		}
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	if m.Type == constants.MaintenanceTypeRepair {
		s.log.Info(fmt.Sprintf(constants.LogRepairDeviceEntered, m.DeviceID, m.OriginalStatus, constants.DeviceStatusUnderMaintenance))
	}
	s.log.Info(fmt.Sprintf(constants.LogMaintenanceCreated, m.RecordNo, m.DeviceID, m.Type, m.Status))
	s.audit.Record(0, operator, "CREATE", "maintenance", util.Uint64String(m.ID), "创建保养/维修工单: "+m.RecordNo, operator, "")
	return m, nil
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

// Start 开始执行工单。
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
		m.Engineer = req.Engineer
		if err := s.repo.UpdateTx(tx, m); err != nil {
			return err
		}
		// 故障维修设备在工单创建时已转为维修中，此处无需再改设备状态。
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
// 故障维修完工必须二选一给出结论：continue_use 继续使用→恢复创建时记录的原状态；
// unrepairable 无法修好→设备转已报废。设备行在事务内加锁，与调拨/报废审批互斥。
func (s *MaintenanceService) Complete(id uint, req *dto.CompleteMaintenanceReq, operator string) (*model.MaintenanceRecord, error) {
	var updated *model.MaintenanceRecord
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		m, err := s.repo.FindByIDForUpdate(tx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "工单不存在: id="+util.Uint64String(id), nil)
		}
		if err != nil {
			return err
		}
		if m.Status != constants.MaintenanceStatusInProgress {
			return util.NewAppError(http.StatusConflict, constants.MsgInvalidStatus, nil)
		}
		outcome := ""
		if m.Type == constants.MaintenanceTypeRepair {
			// 故障维修必须显式选择完工结论，防止完工时设备状态被改错。
			switch req.RepairOutcome {
			case constants.RepairOutcomeContinueUse, constants.RepairOutcomeUnrepairable:
				outcome = req.RepairOutcome
			default:
				return util.NewAppError(http.StatusBadRequest,
					"故障维修完工必须选择维修结论(repair_outcome): continue_use=继续使用, unrepairable=无法修好", nil)
			}
			d, err := s.device.FindByIDForUpdate(tx, m.DeviceID)
			if errors.Is(err, repository.ErrNotFound) {
				return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(m.DeviceID), nil)
			}
			if err != nil {
				return err
			}
			// 设备已不在维修中，说明状态被其他流程改动，后到的完工必须看到最新状态。
			if d.Status != constants.DeviceStatusUnderMaintenance {
				return util.NewAppError(http.StatusConflict,
					"设备当前状态为「"+util.DeviceStatusText(d.Status)+"」，"+constants.MsgDeviceStatusChanged+"（设备名: "+d.Name+"）", nil)
			}
		}
		now := time.Now()
		m.Status = constants.MaintenanceStatusCompleted
		m.ExecutedDate = &now
		m.Content = req.Content
		m.ReplacedParts = req.ReplacedParts
		m.WorkHours = req.WorkHours
		m.Cost = req.Cost
		m.RepairResult = req.RepairResult
		m.RepairOutcome = outcome
		if err := s.repo.UpdateTx(tx, m); err != nil {
			return err
		}
		// 按完工结论变更设备状态：无法修好→已报废；其余→恢复创建时记住的原状态。
		if m.Type == constants.MaintenanceTypeRepair {
			target := m.OriginalStatus
			if outcome == constants.RepairOutcomeUnrepairable {
				target = constants.DeviceStatusScrapped
			}
			if target == "" {
				// 兼容历史数据：未记录原状态时默认恢复为使用中。
				target = constants.DeviceStatusInUse
			}
			if err := s.device.UpdateStatusTx(tx, m.DeviceID, target); err != nil {
				return err
			}
			if outcome == constants.RepairOutcomeUnrepairable {
				s.log.Info(fmt.Sprintf(constants.LogRepairDeviceScrapped, m.DeviceID, m.RecordNo, target))
			} else {
				s.log.Info(fmt.Sprintf(constants.LogRepairDeviceRestored, m.DeviceID, m.OriginalStatus, target, m.RecordNo))
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
	auditDetail := "完成工单: " + updated.RecordNo
	if updated.Type == constants.MaintenanceTypeRepair {
		auditDetail += " 完工结论: " + util.RepairOutcomeText(updated.RepairOutcome)
	}
	s.audit.Record(0, operator, "COMPLETE", "maintenance", util.Uint64String(updated.ID), auditDetail, operator, "")
	return updated, nil
}

// Cancel 取消工单。待处理或执行中的工单均可取消；
// 故障维修取消时将设备恢复为创建时记录的原状态（设备行加锁，与调拨/报废审批互斥）。
func (s *MaintenanceService) Cancel(id uint, req *dto.CancelMaintenanceReq, operator string) (*model.MaintenanceRecord, error) {
	var updated *model.MaintenanceRecord
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		m, err := s.repo.FindByIDForUpdate(tx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "工单不存在: id="+util.Uint64String(id), nil)
		}
		if err != nil {
			return err
		}
		if m.Status != constants.MaintenanceStatusPending && m.Status != constants.MaintenanceStatusInProgress {
			return util.NewAppError(http.StatusConflict, constants.MsgInvalidStatus, nil)
		}
		m.Status = constants.MaintenanceStatusCancelled
		if req.Reason != "" {
			m.RepairResult = "取消原因: " + req.Reason
		}
		// 故障维修：取消后恢复设备原状态。仅当设备当前确为维修中时才恢复，避免覆盖其他流程已变更的状态。
		if m.Type == constants.MaintenanceTypeRepair {
			d, err := s.device.FindByIDForUpdate(tx, m.DeviceID)
			if errors.Is(err, repository.ErrNotFound) {
				return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(m.DeviceID), nil)
			}
			if err != nil {
				return err
			}
			if d.Status == constants.DeviceStatusUnderMaintenance {
				target := m.OriginalStatus
				if target == "" {
					// 兼容历史数据：未记录原状态时默认恢复为使用中。
					target = constants.DeviceStatusInUse
				}
				if err := s.device.UpdateStatusTx(tx, m.DeviceID, target); err != nil {
					return err
				}
				s.log.Info(fmt.Sprintf(constants.LogRepairDeviceRestored, m.DeviceID, m.OriginalStatus, target, m.RecordNo))
			}
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
