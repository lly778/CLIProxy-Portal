package backup

import (
	"context"
	"log"
	"path/filepath"
	"time"
)

// Only fixed stage names reach status files. Docker output, restored values and
// credentials must never become progress messages. Reporting is observational:
// a status write failure must not interrupt a data switch or its rollback.
var restorePhases = map[string]string{
	"preflight":           "检查恢复请求与后台策略",
	"decrypt":             "解密并解压备份",
	"verify_archive":      "校验备份清单、文件及数据库",
	"validate_deployment": "核对 CPA / CPAMP 部署模板、镜像版本与管理配置",
	"download_images":     "下载 CPA / CPAMP 指定版本镜像（已有镜像层会复用）",
	"prepare_volume":      "检查并准备 CPAMP 数据卷",
	"verify_targets":      "核对当前服务、恢复目标与磁盘空间",
	"stage_data":          "准备恢复数据、备份设置并清理旧登录会话",
	"stop_services":       "停止需要恢复的服务，即将短暂停服",
	"rollback_snapshot":   "保存覆盖前的数据回退副本",
	"replace_data":        "替换数据库与所选配置",
	"start_services":      "启动恢复后的服务",
	"rebuild_services":    "重建并启动 CPA / CPAMP",
	"start_portal":        "启动门户",
	"health_check":        "检查恢复后的服务健康（重建模式同时验证管理连接）",
	"rollback":            "恢复失败，正在回退原数据及服务",
}

type restoreProgressKey struct{}

func reportRestorePhase(ctx context.Context, phase string) {
	if _, ok := restorePhases[phase]; !ok {
		return
	}
	if report, ok := ctx.Value(restoreProgressKey{}).(func(string)); ok {
		report(phase)
	}
}

func withRestoreProgress(ctx context.Context, dir string, status *RestoreStatus) context.Context {
	return context.WithValue(ctx, restoreProgressKey{}, func(phase string) {
		if status.Phase == phase {
			return
		}
		if phase == "rollback" {
			status.FailedPhase = status.Phase
		}
		status.Phase, status.PhaseStartedAt = phase, time.Now().UTC()
		status.Message = "正在" + restorePhases[phase]
		if phase == "rollback" {
			status.Message = restorePhases[phase]
		}
		if err := writeJSON(filepath.Join(dir, "restore-status.json"), status); err != nil {
			log.Print("restore progress state could not be saved")
		}
	})
}
