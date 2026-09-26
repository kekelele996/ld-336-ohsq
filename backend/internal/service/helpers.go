package service

import (
	"errors"
	"net/http"

	"github.com/medasset/medasset/internal/constants"
	"github.com/medasset/medasset/internal/model"
	"github.com/medasset/medasset/internal/util"
)

// wrapSvcErr 统一包装 service 层错误，保留 AppError 链。
func wrapSvcErr(err error) error {
	var appErr *util.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return util.NewAppError(http.StatusInternalServerError, constants.MsgInternalError, err)
}

// rejectByDeviceStatus 按设备最新状态判断调拨/报废是否允许继续。
// 返回的错误信息包含设备名、当前状态文本，便于“后到的一方”看到最新状态并收到提示。
func rejectByDeviceStatus(d *model.Device, action string) error {
	switch d.Status {
	case constants.DeviceStatusScrapped:
		return util.NewAppError(http.StatusConflict,
			"设备「"+d.Name+"」已报废，"+action+"不可继续（当前状态: "+util.DeviceStatusText(d.Status)+"）", nil)
	case constants.DeviceStatusUnderMaintenance:
		return util.NewAppError(http.StatusConflict,
			"设备「"+d.Name+"」正在维修中，"+action+"已被拦截，请等待维修结束后刷新重试（当前状态: "+util.DeviceStatusText(d.Status)+"）", nil)
	default:
		return nil
	}
}

// rejectIfStatusChangedSinceSnapshot 检测设备状态在审批事务等待行锁期间是否已被其他流程（维修完工/取消）修改。
// snapshot 是加锁前的快照读结果，latest 是拿到行锁后读到的最新状态；两者不一致即说明后到一方
// 依据的是过期页面，必须拒绝并提示用户刷新查看最新状态。
func rejectIfStatusChangedSinceSnapshot(snapshot, latest *model.Device, action string) error {
	if snapshot.Status != latest.Status {
		return util.NewAppError(http.StatusConflict,
			"设备「"+latest.Name+"」状态刚被维修流程更新（"+util.DeviceStatusText(snapshot.Status)+" → "+
				util.DeviceStatusText(latest.Status)+"），"+action+"未执行，请刷新查看最新状态后再处理", nil)
	}
	return nil
}
