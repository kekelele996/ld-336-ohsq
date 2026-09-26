package service

import (
	"errors"
	"net/http"
	"time"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/model"
	"github.com/medasset/medasset/internal/repository"
	"github.com/medasset/medasset/internal/util"
	"gorm.io/gorm"
)

// 申请单类型文案（用于并发冲突提示，包含实体名与字段名）。
const (
	entityTransfer = "调拨申请"
	entityScrap    = "报废申请"
)

// wrapSvcErr 统一包装 service 层错误，保留 AppError 链。
func wrapSvcErr(err error) error {
	var appErr *util.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
}

// ensureDeviceCurrentStatus 校验设备当前状态是否符合预期；
// 不符时返回 409，并把设备最新状态文案带给后到的一方（并发提示）。
func ensureDeviceCurrentStatus(d *model.Device, want string) error {
	if d.Status == want {
		return nil
	}
	return util.NewAppError(http.StatusConflict,
		constants.MsgDeviceStatusChanged+"（设备: "+d.Name+"，当前状态: "+util.DeviceStatusText(d.Status)+"）", nil)
}

// rejectIfDeviceBusy 维修中/已报废设备不允许发起或批准调拨、报废申请。
func rejectIfDeviceBusy(d *model.Device, action, operator string, log func(string, ...any)) error {
	switch d.Status {
	case constants.DeviceStatusUnderMaintenance:
		log("设备维修中操作被拦截",
			"device_id", d.ID, "action", action, "status", d.Status, "operator", operator)
		return util.NewAppError(http.StatusConflict,
			constants.MsgDeviceInMaintenance+"（设备: "+d.Name+"，当前状态: "+util.DeviceStatusText(d.Status)+"）", nil)
	case constants.DeviceStatusScrapped:
		return util.NewAppError(http.StatusConflict,
			constants.MsgDeviceInScrapped+"（设备: "+d.Name+"）", nil)
	}
	return nil
}

// restoredDeviceStatus 故障维修结束后恢复的设备状态；原状态缺失时回退为使用中。
func restoredDeviceStatus(previous string) string {
	if previous == "" || previous == constants.DeviceStatusUnderMaintenance {
		return constants.DeviceStatusInUse
	}
	return previous
}

// rejectIfRepairedSinceRequest 审批调拨/报废时，在已持有设备行锁的前提下，
// 用当前读校验“申请提交后设备是否进入过有效故障维修流程（待处理/处理中/已完工）”。
// 这是维修完工与审批前后脚提交时的最终互斥屏障：
//   - 若维修尚未结束：rejectIfDeviceBusy 已按“维修中”拦截；
//   - 若维修刚完工（设备看似已恢复）：本检查仍能发现申请之后发生过维修，
//     后到的审批一方收到 409、看到设备最新状态，并需刷新后重新发起申请。
//
// 已取消的维修单（设备已恢复原状态）不影响既有申请。
func rejectIfRepairedSinceRequest(
	tx *gorm.DB,
	maintRepo *repository.MaintenanceRepository,
	d *model.Device,
	submittedAt time.Time,
	entityName string,
) error {
	repair, err := maintRepo.FindRepairSinceTx(tx, d.ID, submittedAt)
	if err != nil {
		return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
	}
	if repair == nil {
		return nil
	}
	return util.NewAppError(http.StatusConflict,
		constants.MsgDeviceStatusChanged+"："+entityName+"提交后设备「"+d.Name+"」已发生故障维修"+
			"（维修单: "+repair.RecordNo+"，当前状态: "+util.DeviceStatusText(d.Status)+
			"），请刷新确认设备最新状态后重新发起申请", nil)
}
