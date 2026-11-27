package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

var ErrProbeBusy = errors.New("该渠道正在探测，请稍后重试")

type Scheduler struct {
	store            *Store
	cfg              Config
	fails            map[string]int
	oks              map[string]int
	latHigh          map[string]bool
	latSeed          map[string]bool
	maintenanceEnded map[string]int64
	nextDue          map[string]time.Time
	running          map[string]bool
	mu               sync.Mutex
	stateMu          sync.Mutex
	cancelFunc       context.CancelFunc
	ctx              context.Context
	done             chan struct{}
	workers          sync.WaitGroup
	sem              chan struct{}
}

func NewScheduler(store *Store, cfg Config) *Scheduler {
	return &Scheduler{
		store: store, cfg: cfg, fails: map[string]int{}, oks: map[string]int{},
		latHigh: map[string]bool{}, latSeed: map[string]bool{}, maintenanceEnded: map[string]int64{},
		nextDue: map[string]time.Time{}, running: map[string]bool{},
		sem: make(chan struct{}, max(1, cfg.Probe.Concurrency)), done: make(chan struct{}),
	}
}

func (s *Scheduler) Start() {
	s.ctx, s.cancelFunc = context.WithCancel(context.Background())
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		cleanup := time.NewTicker(time.Hour)
		defer cleanup.Stop()
		s.RunOnce(s.ctx)
		for {
			select {
			case <-s.ctx.Done():
				s.workers.Wait()
				return
			case <-ticker.C:
				s.RunOnce(s.ctx)
			case <-cleanup.C:
				if err := s.store.Cleanup(s.cfg.Storage.RetentionDays); err != nil {
					log.Printf("cleanup failed: %v", err)
				}
			}
		}
	}()
}

func (s *Scheduler) Stop() {
	if s.cancelFunc != nil {
		s.cancelFunc()
		<-s.done
	}
}

func (s *Scheduler) intervalFor(ch Channel) time.Duration {
	seconds := cfgInt(ch.Config, "interval_seconds", s.cfg.Probe.IntervalSeconds)
	return time.Duration(max(10, seconds)) * time.Second
}

func (s *Scheduler) reserve(ch Channel, now time.Time, force bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[ch.ID] || (!force && now.Before(s.nextDue[ch.ID])) {
		return false
	}
	s.running[ch.ID] = true
	return true
}

func (s *Scheduler) release(ch Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, ch.ID)
	s.nextDue[ch.ID] = time.Now().Add(s.intervalFor(ch))
}

func (s *Scheduler) Forget(id string) {
	s.mu.Lock()
	delete(s.nextDue, id)
	s.mu.Unlock()
	s.stateMu.Lock()
	delete(s.fails, id)
	delete(s.oks, id)
	delete(s.latHigh, id)
	delete(s.latSeed, id)
	s.stateMu.Unlock()
}

func (s *Scheduler) RunOnce(ctx context.Context) {
	channels, err := s.store.ListChannels(true)
	if err != nil {
		log.Printf("list channels failed: %v", err)
		return
	}
	for _, ch := range channels {
		if ctx.Err() != nil || !s.reserve(ch, time.Now(), false) {
			continue
		}
		s.workers.Add(1)
		go func(ch Channel) {
			defer s.workers.Done()
			defer s.release(ch)
			if _, err := s.probe(ctx, ch); err != nil && ctx.Err() == nil {
				log.Printf("probe %s: %v", ch.ID, err)
			}
		}(ch)
	}
}

func (s *Scheduler) ProbeNow(ctx context.Context, ch Channel) (ProbeResult, error) {
	if !s.reserve(ch, time.Now(), true) {
		return ProbeResult{}, ErrProbeBusy
	}
	defer s.release(ch)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.ctx != nil {
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
	}
	return s.probe(ctx, ch)
}

func (s *Scheduler) probe(ctx context.Context, ch Channel) (ProbeResult, error) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ProbeResult{}, ctx.Err()
	}
	startedAt := s.store.now().Unix()
	result := ProbeChannel(ctx, ch, time.Duration(max(1, s.cfg.Probe.TimeoutSeconds))*time.Second)
	// A cancelled shutdown or browser request is not a failed service probe.
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	// Serialize maintenance changes with result classification, without holding the lock during network I/O.
	s.store.maintenanceMu.RLock()
	locked := true
	defer func() {
		if locked {
			s.store.maintenanceMu.RUnlock()
		}
	}()
	finishedAt := s.store.now().Unix()
	maintenance, err := s.store.MaintenanceForProbe(ch.ID, startedAt, finishedAt)
	if err != nil {
		return result, err
	}
	var maintenanceID *int64
	if maintenance != nil {
		maintenanceID = &maintenance.ID
	}
	// Restore the previous latency threshold state before this probe is stored,
	// otherwise a restart re-emits latency alerts for every channel.
	if err := s.seedLatency(ch.ID); err != nil {
		log.Printf("restore latency state for %s: %v", ch.ID, err)
	}
	if err := s.store.addProbeWithMaintenance(ch.ID, result, maintenanceID); err != nil {
		return result, err
	}
	if maintenance != nil {
		s.resetAlertState(ch.ID)
		return result, nil
	}
	if err := s.resetAfterMaintenance(ch.ID, finishedAt); err != nil {
		return result, err
	}
	event, detail, err := s.updateState(ch, result)
	if err != nil {
		return result, err
	}
	latEvent, latDetail := s.checkLatency(ch, result)
	s.store.maintenanceMu.RUnlock()
	locked = false
	// A window may be created while the probe is being processed.
	maintenance, err = s.store.MaintenanceForProbe(ch.ID, startedAt, s.store.now().Unix())
	if err != nil {
		return result, err
	}
	if maintenance != nil {
		s.resetAlertState(ch.ID)
		return result, nil
	}
	if s.cfg.Alerts.Enabled {
		if event != "" {
			if err := SendAlert(ctx, s.cfg.Alerts, event, ch, detail); err != nil {
				log.Printf("alert for %s failed: %v", ch.ID, err)
			}
		}
		if latEvent != "" {
			if err := SendAlert(ctx, s.cfg.Alerts, latEvent, ch, latDetail); err != nil {
				log.Printf("latency alert for %s failed: %v", ch.ID, err)
			}
		}
	}
	return result, nil
}

func (s *Scheduler) resetAlertState(id string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.fails[id], s.oks[id] = 0, 0
	delete(s.latHigh, id)
}

func (s *Scheduler) resetAfterMaintenance(id string, now int64) error {
	end, err := s.store.LatestMaintenanceEnd(id, now)
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if end > s.maintenanceEnded[id] {
		delete(s.fails, id)
		delete(s.oks, id)
		delete(s.latHigh, id)
		s.maintenanceEnded[id] = end
	}
	return nil
}

// seedLatency restores the high-latency state from the last stored probe so that
// process restarts do not repeat alerts that were already delivered.
func (s *Scheduler) seedLatency(id string) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if _, seen := s.latSeed[id]; seen {
		return nil
	}
	s.latSeed[id] = true
	threshold := s.cfg.Alerts.LatencyThresholdMs
	if threshold <= 0 {
		return nil
	}
	lat, err := s.store.LastProbeLatency(id)
	if err != nil {
		delete(s.latSeed, id)
		return err
	}
	if lat != nil {
		s.latHigh[id] = *lat >= float64(threshold)
	}
	return nil
}

// checkLatency 监测成功探测的响应耗时跨越阈值（变慢/恢复），返回告警事件。
func (s *Scheduler) checkLatency(ch Channel, result ProbeResult) (string, string) {
	threshold := s.cfg.Alerts.LatencyThresholdMs
	if !result.OK || result.LatencyMs == nil || math.IsNaN(*result.LatencyMs) || math.IsInf(*result.LatencyMs, 0) || *result.LatencyMs < 0 {
		return "", ""
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if threshold <= 0 {
		delete(s.latHigh, ch.ID)
		return "", ""
	}
	high := *result.LatencyMs >= float64(threshold)
	prev := s.latHigh[ch.ID]
	if high == prev {
		return "", ""
	}
	s.latHigh[ch.ID] = high
	if high {
		return "latency", fmt.Sprintf("响应耗时 %v ms 达到或超过阈值 %d ms", round1(*result.LatencyMs), threshold)
	}
	return "latency-recovered", fmt.Sprintf("响应耗时 %v ms 已恢复到阈值 %d ms 以内", round1(*result.LatencyMs), threshold)
}

func (s *Scheduler) updateState(ch Channel, result ProbeResult) (string, string, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if _, loaded := s.fails[ch.ID]; !loaded {
		fails, oks, err := s.store.probeStreakSince(ch.ID, s.maintenanceEnded[ch.ID])
		if err != nil {
			return "", "", err
		}
		s.fails[ch.ID], s.oks[ch.ID] = fails, oks
	} else if result.OK {
		s.fails[ch.ID] = 0
		s.oks[ch.ID]++
	} else {
		s.oks[ch.ID] = 0
		s.fails[ch.ID]++
	}
	open, err := s.store.OpenIncidentFor(ch.ID)
	if err != nil {
		return "", "", err
	}
	if result.OK && open != nil && s.oks[ch.ID] >= max(1, s.cfg.Alerts.RecoverThreshold) {
		if err := s.store.ResolveIncident(ch.ID); err != nil {
			return "", "", err
		}
		return "recovered", fmt.Sprintf("连续 %d 次探测成功", s.oks[ch.ID]), nil
	}
	if !result.OK && open == nil && s.fails[ch.ID] >= max(1, s.cfg.Alerts.FailThreshold) {
		reason := result.Error
		if reason == "" {
			reason = "probe failed"
		}
		if _, err := s.store.OpenIncident(ch.ID, groupOr(ch.Group), reason); err != nil {
			return "", "", err
		}
		return "down", fmt.Sprintf("连续 %d 次失败: %s", s.fails[ch.ID], reason), nil
	}
	return "", "", nil
}
