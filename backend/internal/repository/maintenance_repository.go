package repository

import (
	"errors"
	"fmt"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MaintenanceRepository 维护保养/维修仓储。
type MaintenanceRepository struct {
	db *gorm.DB
}

func NewMaintenanceRepository(db *gorm.DB) *MaintenanceRepository {
	return &MaintenanceRepository{db: db}
}

// DB 返回底层数据库句柄（供 service 层开启事务）。
func (r *MaintenanceRepository) DB() *gorm.DB { return r.db }

// Create 创建保养/维修工单。
func (r *MaintenanceRepository) Create(m *model.MaintenanceRecord) error {
	return r.db.Create(m).Error
}

// CreateBatch 批量创建保养计划工单（事务内使用）。
func (r *MaintenanceRepository) CreateBatch(tx *gorm.DB, records []model.MaintenanceRecord) error {
	if len(records) == 0 {
		return nil
	}
	return tx.Create(&records).Error
}

// FindByID 按 ID 查询。
func (r *MaintenanceRepository) FindByID(id uint) (*model.MaintenanceRecord, error) {
	var m model.MaintenanceRecord
	err := r.db.First(&m, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &m, err
}

// FindByIDForUpdate 加锁查询。
func (r *MaintenanceRepository) FindByIDForUpdate(tx *gorm.DB, id uint) (*model.MaintenanceRecord, error) {
	var m model.MaintenanceRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&m, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &m, err
}

// FindByIDTx 事务内非加锁查询（用于事务内预读基础信息）。
func (r *MaintenanceRepository) FindByIDTx(tx *gorm.DB, id uint) (*model.MaintenanceRecord, error) {
	var m model.MaintenanceRecord
	err := tx.First(&m, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &m, err
}

// List 分页查询保养/维修记录。
func (r *MaintenanceRepository) List(page, pageSize int, deviceID uint, mType, status string) ([]model.MaintenanceRecord, int64, error) {
	var list []model.MaintenanceRecord
	var total int64
	q := r.db.Model(&model.MaintenanceRecord{})
	if deviceID > 0 {
		q = q.Where("device_id = ?", deviceID)
	}
	if mType != "" {
		q = q.Where("type = ?", mType)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := q.Order("id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error
	return list, total, err
}

// Update 更新工单。
func (r *MaintenanceRepository) Update(m *model.MaintenanceRecord) error {
	return r.db.Save(m).Error
}

// UpdateTx 在指定事务中更新更新工单。
func (r *MaintenanceRepository) UpdateTx(tx *gorm.DB, m *model.MaintenanceRecord) error {
	return tx.Save(m).Error
}

// SumCost 统计维修成本（被列表与统计接口复用）。
func (r *MaintenanceRepository) SumCost() (float64, error) {
	var sum float64
	err := r.db.Model(&model.MaintenanceRecord{}).Select("COALESCE(SUM(cost),0)").Scan(&sum).Error
	return sum, err
}

// CountByType 按类型统计工单数。
func (r *MaintenanceRepository) CountByType(mType string) (int64, error) {
	var n int64
	err := r.db.Model(&model.MaintenanceRecord{}).Where("type = ?", mType).Count(&n).Error
	return n, err
}

// IsRecordNoTaken 判断RecordNo是否已存在。
func (r *MaintenanceRepository) IsRecordNoTaken(v string) (bool, error) {
	var n int64
	if err := r.db.Model(&model.MaintenanceRecord{}).Where("record_no = ?", v).Count(&n).Error; err != nil {
		return false, fmt.Errorf("check record_no: %w", err)
	}
	return n > 0, nil
}

// HasActiveRepairTx 判断设备在事务内是否存在未结束（待处理/处理中）的故障维修单。
func (r *MaintenanceRepository) HasActiveRepairTx(tx *gorm.DB, deviceID uint) (bool, error) {
	var n int64
	err := tx.Model(&model.MaintenanceRecord{}).
		Where("device_id = ? AND type = ? AND status IN ?",
			deviceID, constants.MaintenanceTypeRepair,
			[]string{constants.MaintenanceStatusPending, constants.MaintenanceStatusInProgress}).
		Count(&n).Error
	if err != nil {
		return false, fmt.Errorf("check active repair: %w", err)
	}
	return n > 0, nil
}

// FindRepairSinceTx 查找设备在指定时间之后创建的、仍然有效（待处理/处理中/已完工）的最近一张
// 故障维修单并加行锁；已取消的工单不影响既有申请。
// 调用方必须先持有设备行锁（FOR UPDATE）；本方法为当前读，能看到并发事务已提交的最新数据，
// 锁顺序固定为 设备→工单，与维修完工/取消流程一致以避免死锁。
func (r *MaintenanceRepository) FindRepairSinceTx(tx *gorm.DB, deviceID uint, since time.Time) (*model.MaintenanceRecord, error) {
	var m model.MaintenanceRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("device_id = ? AND type = ? AND status IN ? AND created_at > ?",
			deviceID, constants.MaintenanceTypeRepair,
			[]string{constants.MaintenanceStatusPending, constants.MaintenanceStatusInProgress, constants.MaintenanceStatusCompleted},
			since).
		Order("id DESC").First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find repair since: %w", err)
	}
	return &m, nil
}
