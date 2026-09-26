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
	"gorm.io/gorm"
)

// ScrapService 设备报废服务。
type ScrapService struct {
	repo   *repository.ScrapRepository
	device *repository.DeviceRepository
	audit  *AuditService
	log    *slog.Logger
}

func NewScrapService(repo *repository.ScrapRepository, device *repository.DeviceRepository, audit *AuditService, log *slog.Logger) *ScrapService {
	return &ScrapService{repo: repo, device: device, audit: audit, log: log}
}

// Create 发起报废申请。已报废或维修中的设备不允许发起（设备行加锁，防止与维修流程并发）。
func (s *ScrapService) Create(req *dto.CreateScrapReq, applicant string) (*model.ScrapRequest, error) {
	sr := &model.ScrapRequest{}
	err := s.repo.DB().Transaction(func(tx *gorm.DB) error {
		d, err := s.device.FindByIDForUpdate(tx, req.DeviceID)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(req.DeviceID), nil)
		}
		if err != nil {
			return err
		}
		if d.Status == constants.DeviceStatusScrapped {
			return util.NewAppError(http.StatusConflict,
				"设备「"+d.Name+"」已报废，报废申请不可重复提交（当前状态: "+util.DeviceStatusText(d.Status)+"）", nil)
		}
		if d.Status == constants.DeviceStatusUnderMaintenance {
			return util.NewAppError(http.StatusConflict,
				"设备「"+d.Name+"」正在维修中，报废申请已被拦截，请等待维修结束后刷新重试（当前状态: "+util.DeviceStatusText(d.Status)+"）", nil)
		}
		*sr = model.ScrapRequest{
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
		return nil
	})
	if err != nil {
		return nil, wrapSvcErr(err)
	}
	s.log.Info(fmt.Sprintf(constants.LogScrapCreated, sr.ScrapNo, sr.DeviceID, sr.Status))
	s.audit.Record(0, applicant, "CREATE", "scrap", util.Uint64String(sr.ID), "发起报废: "+sr.ScrapNo, applicant, "")
	return sr, nil
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
// 审批时对设备行加锁并按最新状态校验：维修中一律拦截；
// 与维修完工并发时双方抢同一把设备行锁，后到者读到最新状态，只会有一方成功。
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
		// 加锁前先做一次快照读：与维修完工并发时识别等待期间的状态变化，
		// 保证两边不会都成功，后到的报废审批看到最新状态并收到提示。
		snapshot, err := s.device.FindByIDTx(tx, sr.DeviceID)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(sr.DeviceID), nil)
		}
		if err != nil {
			return err
		}
		d, err := s.device.FindByIDForUpdate(tx, sr.DeviceID)
		if errors.Is(err, repository.ErrNotFound) {
			return util.NewAppError(http.StatusNotFound, "设备不存在: device_id="+util.Uint64String(sr.DeviceID), nil)
		}
		if err != nil {
			return err
		}
		if err := rejectIfStatusChangedSinceSnapshot(snapshot, d, "报废审批"); err != nil {
			s.log.Warn(fmt.Sprintf(constants.LogScrapBlockedByDevice, sr.ScrapNo, d.ID, d.Status, "stale_approve", operator))
			return err
		}
		if d.Status == constants.DeviceStatusUnderMaintenance {
			s.log.Warn(fmt.Sprintf(constants.LogScrapBlockedByDevice, sr.ScrapNo, d.ID, d.Status, "approve", operator))
			return util.NewAppError(http.StatusConflict,
				"设备「"+d.Name+"」正在维修中，报废审批已被拦截，请等待维修结束后刷新重试（当前状态: "+util.DeviceStatusText(d.Status)+"）", nil)
		}
		if d.Status == constants.DeviceStatusScrapped {
			return util.NewAppError(http.StatusConflict,
				"设备「"+d.Name+"」已报废，不可重复审批（当前状态: "+util.DeviceStatusText(d.Status)+"）", nil)
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

// Reject 驳回报废申请。
func (s *ScrapService) Reject(id uint, req *dto.ScrapApproveReq, operator string) (*model.ScrapRequest, error) {
	sr, err := s.repo.FindByID(id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, util.NewAppError(http.StatusNotFound, "报废申请不存在: id="+util.Uint64String(id), nil)
	}
	if err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	if sr.Status != constants.ScrapStatusPending {
		return nil, util.NewAppError(http.StatusConflict, constants.MsgInvalidStatus, nil)
	}
	sr.Status = constants.ScrapStatusRejected
	sr.Approver = operator
	sr.ApproveComment = req.Comment
	now := time.Now()
	sr.ApproveAt = &now
	if err := s.repo.Update(sr); err != nil {
		return nil, util.NewAppError(http.StatusInternalServerError, "驳回报废失败: scrap_no="+sr.ScrapNo, err)
	}
	s.log.Info(fmt.Sprintf(constants.LogScrapRejected, sr.ScrapNo, operator, req.Comment))
	s.audit.Record(0, operator, "REJECT", "scrap", util.Uint64String(sr.ID), "报废驳回: "+sr.ScrapNo, operator, "")
	return sr, nil
}
