package main

import (
	"encoding/json"
	"testing"
)

func TestCompatStatusShape(t *testing.T) {
	snap := &StatusResponse{
		GeneratedAt:   1700000000,
		WindowSeconds: 27000,
		BucketSeconds: 300,
		Site:          SiteInfo{Title: "T", Description: "D"},
		Channels: []ChannelStatus{
			{ID: "gpt", Name: "GPT", Group: "GPT", Type: "openai", CurrentStatus: "down",
				Availability: ptrFloat(50),
				Buckets: []Bucket{
					{TS: 1000, Status: "ok", Probes: 2, OkProbes: 2, At: 1000, DurationSeconds: 300,
						TTFTMs: ptrFloat(1234)},
					{TS: 1300, Status: "down", Probes: 1, OkProbes: 0, At: 1300, DurationSeconds: 300},
				}},
			{ID: "site", Name: "官网", Group: "站点", Type: "http", CurrentStatus: "nodata"},
		},
	}
	raw, err := json.Marshal(CompatStatus(snap))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Site          struct{ Title, Footer string } `json:"site"`
		Overall       string                         `json:"overall"`
		Summary       string                         `json:"summary"`
		WindowMinutes int                            `json:"window_minutes"`
		BucketMinutes int                            `json:"bucket_minutes"`
		Groups        []struct {
			ID         string   `json:"id"`
			Category   string   `json:"category"`
			Multiplier int      `json:"multiplier"`
			Status     string   `json:"status"`
			Uptime     *float64 `json:"uptime"`
			Points     []struct {
				At              int64    `json:"at"`
				DurationSeconds int      `json:"duration_seconds"`
				Status          string   `json:"status"`
				Total           int      `json:"total"`
				Failures        int      `json:"failures"`
				TTFTMs          *float64 `json:"ttft_ms"`
				Source          string   `json:"source"`
			} `json:"points"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Site.Footer != "D" || out.Site.Title != "T" {
		t.Fatalf("site = %+v", out.Site)
	}
	if out.Overall != "red" {
		t.Fatalf("overall = %q, want red (one down channel)", out.Overall)
	}
	if out.WindowMinutes != 450 || out.BucketMinutes != 5 {
		t.Fatalf("minutes = %d/%d", out.WindowMinutes, out.BucketMinutes)
	}
	g := out.Groups[0]
	if g.Category != "GPT" || g.Multiplier != 1 || g.Status != "red" || g.Uptime == nil || *g.Uptime != 50 {
		t.Fatalf("group0 = %+v", g)
	}
	if len(g.Points) != 2 {
		t.Fatalf("points = %d", len(g.Points))
	}
	p0 := g.Points[0]
	if p0.Status != "green" || p0.Total != 2 || p0.Failures != 0 || p0.TTFTMs == nil || *p0.TTFTMs != 1234 || p0.Source != "probe" {
		t.Fatalf("point0 = %+v", p0)
	}
	if g.Points[1].Status != "red" || g.Points[1].Failures != 1 {
		t.Fatalf("point1 = %+v", g.Points[1])
	}
	if out.Groups[1].Status != "unknown" {
		t.Fatalf("nodata group status = %q", out.Groups[1].Status)
	}
}

func TestCompatDynamicsIDs(t *testing.T) {
	list := []Dynamic{
		{ID: "d1", Kind: KindNotice, Title: "t", State: StatePublish, Services: []string{"GPT"}},
		{ID: "i7", Kind: KindIncident, Title: "x", State: StateActive,
			Updates: []DynamicUpdate{{At: 5, Label: "已定位", Body: "b"}}},
	}
	raw, err := json.Marshal(CompatDynamics(list))
	if err != nil {
		t.Fatal(err)
	}
	var out []struct {
		ID       int64    `json:"id"`
		Ref      string   `json:"ref"`
		State    string   `json:"state"`
		Services []string `json:"services"`
		Updates  []struct {
			At    int64  `json:"at"`
			Label string `json:"label"`
		} `json:"updates"`
		EndAt any `json:"end_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0].ID != 1 || out[0].Ref != "d1" || len(out[0].Services) != 1 {
		t.Fatalf("d1 compat = %+v", out[0])
	}
	if out[1].ID != 1000007 || out[1].Ref != "i7" {
		t.Fatalf("i7 compat = %+v", out[1])
	}
	if len(out[1].Updates) != 1 || out[1].Updates[0].Label != "已定位" {
		t.Fatalf("updates = %+v", out[1].Updates)
	}
	if out[1].EndAt != nil {
		t.Fatalf("active incident end_at = %v", out[1].EndAt)
	}
}

func ptrFloat(v float64) *float64 { return &v }
