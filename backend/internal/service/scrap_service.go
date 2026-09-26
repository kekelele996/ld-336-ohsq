package service

import (
	"fmt"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/dto"
	"github.com/medasset/medasset/internal/model"
	"github.com/medasset/medasset/internal/repository"
	"github.com/medasset/medasset/internal/util"
	"gorm.io/gorm"
)

// ScrapService 设备报废服务。
type ScrapService struct {
	repo        *repository.ScrapRepository
	device      *repository.DeviceRepository
	maintenance *repository.MaintenanceRepository
	audit       *AuditService
	log         *slog.Logger
}

func NewScrapService(repo *repository.ScrapRepository, device *repository.DeviceRepository, maintenance *repository.MaintenanceRepository, audit *AuditService, log *slog.Logger) *ScrapService {
	return &ScrapService{repo: repo, device: device, maintenance: maintenance, audit: audit, log: log}
}

// Create 发起报废申请。维修中/已报废设备不允许发起报废。
func (s *ScrapService) Create(req *dto.CreateScrapReq, applicant string) (*model.ScrapRequest, error) {
	var created *model.ScrapRequest
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		d, err := s.device.FindByIDForUpdate(tx, req.DeviceID)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(req.DeviceID), nil)
		}
		if err != nil {
			return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
		}
		if err := rejectIfDeviceBusy(d, "create_scrap", applicant, s.log.Info); err != nil {
			return err
		}
		sr := &model.ScrapRequest{
			ScrapNo:        util.GenSerial("SC"),
			DeviceID:       d.ID,
			DeviceName:     d.Name,
			Reason:         req.Reason,
			EstimatedValue: req.EstimatedValue,
			Status:         constants.ScrapStatusPending,
			Applicant:      applicant,
		}
		if err := tx.Create(sr).Error; err != nil {
			return util.NewAppError(http.StatusInternalServerError, "创建报废申请失败: device_name="+d.Name, err)
		}
		created = sr
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogScrapCreated, created.ScrapNo, created.DeviceID, created.Status))
	s.audit.Record(0, applicant, "CREATE", "scrap", util.Uint64String(created.ID), "发起报废: "+created.ScrapNo, applicant, "")
	return created, nil
}

// List 分页查询报废申请。
func (s *ScrapService) List(page, pageSize int, status string) (*util.PageResult, error) {
	list, total, err := s.repo.List(page, pageSize, status)
	if err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	return &util.PageResult{List: list, Total: total, Page: page, PageSize: pageSize}, nil
}

// Approve 审批通过：设备状态变更为已报废并归档。
// 先锁申请单再锁设备，维修中设备一律拦截；
// 与维修完工并发提交时两边只可能一边成功，后到一方看到最新设备状态并收到提示。
func (s *ScrapService) Approve(id uint, req *dto.ScrapApproveReq, operator string) (*model.ScrapRequest, error) {
	var updated *model.ScrapRequest
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		sr, err := s.repo.FindByIDForUpdate(tx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "报废申请不存在: id="+util.Uint64String(id), nil)
		}
		if err != nil {
			return err
		}
		if sr.Status != constants.ScrapStatusPending {
			return util.NewAppError(http.StatusConflict, constants.MsgInvalidStatus, nil)
		}
		d, err := s.device.FindByIDForUpdate(tx, sr.DeviceID)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(sr.DeviceID), nil)
		}
		if err != nil {
			return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
		}
		// 审批前以最新设备状态为准：维修中不可批准，已报废给出最新状态提示。
		if err := rejectIfDeviceBusy(d, "approve_scrap", operator, s.log.Info); err != nil {
			return err
		}
		// 并发互斥屏障：申请提交后设备若进入过有效维修流程，本笔报废需重新发起。
		if err := rejectIfRepairedSinceRequest(tx, s.maintenance, d, sr.CreatedAt, entityScrap); err != nil {
			return err
		}
		sr.Status = constants.ScrapStatusApproved
		sr.Approver = operator
		sr.ApproveComment = req.Comment
		now := time.Now()
		sr.ApproveAt = &now
		if err := s.repo.UpdateTx(tx, sr); err != nil {
			return err
		}
		if err := s.device.UpdateStatusTx(tx, sr.DeviceID, constants.DeviceStatusScrapped); err != nil {
			return err
		}
		updated = sr
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogScrapApproved, updated.ScrapNo, updated.DeviceID, updated.Status))
	s.audit.Record(0, operator, "APPROVE", "scrap", util.Uint64String(updated.ID), "报废通过并归档: "+updated.ScrapNo, operator, "")
	return updated, nil
}

// Reject 驳回报废申请。驳回同样在事务内锁定申请单，避免并发重复审批。
func (s *ScrapService) Reject(id uint, req *dto.ScrapApproveReq, operator string) (*model.ScrapRequest, error) {
	var updated *model.ScrapRequest
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		sr, err := s.repo.FindByIDForUpdate(tx, id)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "报废申请不存在: id="+util.Uint64String(id), nil)
		}
		if err != nil {
			return err
		}
		if sr.Status != constants.ScrapStatusPending {
			return util.NewAppError(http.StatusConflict, constants.MsgInvalidStatus, nil)
		}
		sr.Status = constants.ScrapStatusRejected
		sr.Approver = operator
		sr.ApproveComment = req.Comment
		now := time.Now()
		sr.ApproveAt = &now
		if err := s.repo.UpdateTx(tx, sr); err != nil {
			return util.NewAppError(http.StatusInternalServerError, "驳回报废失败: scrap_no="+sr.ScrapNo, err)
		}
		updated = sr
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogScrapRejected, updated.ScrapNo, operator, req.Comment))
	s.audit.Record(0, operator, "REJECT", "scrap", util.Uint64String(updated.ID), "报废驳回: "+updated.ScrapNo, operator, "")
	return updated, nil
}
