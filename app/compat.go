package main

import "fmt"

// lcapi 兼容层：把我们的响应映射成 status.lcapi.online 的字段形状，
// 供对接它们风格的前端或脚本使用。原字段保持不变，两边互不影响。

var lcStatusText = map[string]string{
	"green":   "正常",
	"yellow":  "异常",
	"red":     "故障",
	"unknown": "无数据",
}

// lcBucketStatus 把内部桶状态映射成参考站的 green/yellow/red/unknown。
func lcBucketStatus(status string) string {
	switch status {
	case "ok":
		return "green"
	case "partial":
		return "yellow"
	case "down":
		return "red"
	}
	return "unknown"
}

// lcOverall 从分组状态推导整体状态。
func lcOverall(groupStatuses []string) string {
	red, yellow, green := 0, 0, 0
	for _, st := range groupStatuses {
		switch st {
		case "red":
			red++
		case "yellow", "unknown":
			yellow++
		case "green":
			green++
		}
	}
	switch {
	case red > 0:
		return "red"
	case yellow > 0:
		return "yellow"
	case green > 0:
		return "green"
	}
	return "yellow"
}

func lcSummary(overall string) string {
	switch overall {
	case "green":
		return "当前所有监控分组运行正常。"
	case "red":
		return "部分分组出现故障，正在排查。"
	default:
		return "部分分组性能下降或暂无数据。"
	}
}

// CompatStatus 输出参考站形状的 /api/status。
func CompatStatus(snap *StatusResponse) map[string]any {
	groups := make([]map[string]any, 0, len(snap.Channels))
	statuses := make([]string, 0, len(snap.Channels))
	for _, ch := range snap.Channels {
		var groupStatus string
		switch ch.CurrentStatus {
		case "ok":
			groupStatus = "green"
		case "down":
			groupStatus = "red"
		case "partial":
			groupStatus = "yellow"
		default:
			groupStatus = "unknown"
		}
		statuses = append(statuses, groupStatus)

		points := make([]map[string]any, 0, len(ch.Buckets))
		for _, b := range ch.Buckets {
			failures := b.Probes - b.OkProbes
			points = append(points, map[string]any{
				"at":               b.TS,
				"duration_seconds": b.DurationSeconds,
				"status":           lcBucketStatus(b.Status),
				"total":            b.Probes,
				"failures":         failures,
				"ttft_ms":          b.TTFTMs,
				"players_avg":      b.PlayersAvg,
				"source":           "probe",
				"probe_count":      b.Probes,
				"probe_failures":   failures,
			})
		}
		groups = append(groups, map[string]any{
			"id":         ch.ID,
			"name":       ch.Name,
			"category":   ch.Group,
			"multiplier": 1,
			"status":     groupStatus,
			"uptime":     ch.Availability,
			"points":     points,
		})
	}
	overall := lcOverall(statuses)
	windowMinutes := snap.WindowSeconds / 60
	return map[string]any{
		"site": map[string]any{
			"title":  snap.Site.Title,
			"footer": snap.Site.Description,
		},
		"generated_at":   snap.GeneratedAt,
		"window_minutes": windowMinutes,
		"bucket_minutes": snap.BucketSeconds / 60,
		"minutes":        windowMinutes,
		"overall":        overall,
		"summary":        fmt.Sprintf("%s %s", lcSummary(overall), snap.Site.Description),
		"groups":         groups,
	}
}

// CompatDynamics 输出参考站形状的 /api/dynamics。
// id 变成数字：人工动态用原编号，自动事件加 1_000_000 偏移避免冲突；
// 原字符串 id 放在 "ref" 字段里，便于回查详情。
func CompatDynamics(list []Dynamic) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, d := range list {
		numericID := numericDynamicID(d.ID)
		updates := make([]map[string]any, 0, len(d.Updates))
		for _, u := range d.Updates {
			updates = append(updates, map[string]any{
				"at":    u.At,
				"label": u.Label,
				"body":  u.Body,
			})
		}
		endAt := any(nil)
		if d.EndAt != nil {
			endAt = *d.EndAt
		}
		services := d.Services
		if services == nil {
			services = []string{}
		}
		out = append(out, map[string]any{
			"id":           numericID,
			"ref":          d.ID,
			"kind":         d.Kind,
			"title":        d.Title,
			"body":         d.Body,
			"state":        d.State,
			"start_at":     d.StartAt,
			"end_at":       endAt,
			"pinned":       d.Pinned,
			"pin_position": d.PinPosition,
			"services":     services,
			"updates":      updates,
			"created_at":   d.CreatedAt,
			"updated_at":   d.UpdatedAt,
		})
	}
	return out
}
