package main

import "strconv"

func maintenanceDynamic(window MaintenanceWindow) Dynamic {
	state := "pending"
	updates := []DynamicUpdate{}
	switch window.State {
	case "active":
		state = StateActive
	case "completed":
		state = StateResolve
		updates = append(updates, DynamicUpdate{At: window.EndAt, Label: "维护窗口结束", Body: "正常告警规则已恢复；服务状态以最新探测结果为准。"})
	case "cancelled":
		state = StateResolve
		label := "维护计划已取消"
		if window.CancelledAt != nil && *window.CancelledAt > window.StartAt {
			label = "维护已提前结束"
		}
		updates = append(updates, DynamicUpdate{At: *window.CancelledAt, Label: label, Body: "此维护窗口不再暂停告警。"})
	}
	services := append([]string{}, window.ChannelIDs...)
	if window.AllChannels {
		services = []string{"全部服务"}
	}
	end := window.effectiveEnd()
	if end < window.StartAt {
		end = window.StartAt
	}
	updated := window.CreatedAt
	if window.State == "cancelled" {
		updated = *window.CancelledAt
	}
	if window.State == "completed" {
		updated = window.EndAt
	}
	return Dynamic{
		ID: "m" + strconv.FormatInt(window.ID, 10), Kind: KindMainten, Title: window.Title, Body: window.Body,
		State: state, StartAt: window.StartAt, EndAt: &end, Services: services, Updates: updates,
		CreatedAt: window.CreatedAt, UpdatedAt: updated, Source: "scheduled",
	}
}
