package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	savingsFlushInterval = 30 * time.Second
	savingsDayRetention  = 7
)

type tokenUsage struct {
	PromptTokens     int64
	CompletionTokens int64
}

type priceQuote struct {
	Reference               string
	PromptMicroPerToken     float64
	CompletionMicroPerToken float64
	Priced                  bool
}

func (q priceQuote) costMicro(usage tokenUsage) int64 {
	if !q.Priced {
		return 0
	}
	cost := float64(usage.PromptTokens)*q.PromptMicroPerToken + float64(usage.CompletionTokens)*q.CompletionMicroPerToken
	return int64(math.Round(cost))
}

type savingsRecord struct {
	CostMicro int64
	Priced    bool
	Reference string
}

type persistedSavingsState struct {
	Since                      time.Time        `json:"since"`
	SavedUSDMicro              int64            `json:"saved_usd_micro"`
	LocalPromptTokens          int64            `json:"local_prompt_tokens"`
	LocalCompletionTokens      int64            `json:"local_completion_tokens"`
	CloudSpendUSDMicro         int64            `json:"cloud_spend_usd_micro"`
	CloudPromptTokens          int64            `json:"cloud_prompt_tokens"`
	CloudCompletionTokens      int64            `json:"cloud_completion_tokens"`
	CloudSpendUSDMicroByUTCDay map[string]int64 `json:"cloud_spend_usd_micro_by_utc_day"`
}

type savingsMeter struct {
	mu                 sync.Mutex
	file               string
	now                func() time.Time
	state              persistedSavingsState
	dirty              bool
	generation         uint64
	usageUnknownTotal  int64
	references         map[string]int64
	cloudUnpricedUsage tokenUsage
	currentSavedMicro  atomic.Int64
	loopStarted        bool
	stopOnce           sync.Once
	stop               chan struct{}
	done               chan struct{}
}

func newSavingsMeter(file string) *savingsMeter {
	m := &savingsMeter{
		file:       file,
		now:        time.Now,
		references: map[string]int64{},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	m.load()
	if file != "" {
		m.loopStarted = true
		go m.flushLoop()
	}
	return m
}

func newSavingsMeterForTest(file string, now func() time.Time) *savingsMeter {
	m := &savingsMeter{
		file:       file,
		now:        now,
		references: map[string]int64{},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	m.load()
	return m
}

func (m *savingsMeter) load() {
	fresh := persistedSavingsState{
		Since:                      m.now().UTC(),
		CloudSpendUSDMicroByUTCDay: map[string]int64{},
	}
	if m.file == "" {
		m.state = fresh
		m.currentSavedMicro.Store(0)
		return
	}
	data, err := os.ReadFile(m.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("bursty savings: state file %s missing; starting fresh", m.file)
		} else {
			log.Printf("bursty savings: read state file %s failed: %v; starting fresh", m.file, err)
		}
		m.state = fresh
		m.currentSavedMicro.Store(0)
		return
	}
	var loaded persistedSavingsState
	if err := json.Unmarshal(data, &loaded); err != nil {
		log.Printf("bursty savings: state file %s is corrupt: %v; starting fresh", m.file, err)
		m.state = fresh
		m.currentSavedMicro.Store(0)
		return
	}
	if loaded.Since.IsZero() {
		loaded.Since = fresh.Since
	}
	if loaded.CloudSpendUSDMicroByUTCDay == nil {
		loaded.CloudSpendUSDMicroByUTCDay = map[string]int64{}
	}
	pruneCloudSpendDays(loaded.CloudSpendUSDMicroByUTCDay, m.now().UTC())
	m.state = loaded
	m.currentSavedMicro.Store(loaded.SavedUSDMicro)
}

func (m *savingsMeter) flushLoop() {
	ticker := time.NewTicker(savingsFlushInterval)
	defer ticker.Stop()
	defer close(m.done)
	for {
		select {
		case <-ticker.C:
			if err := m.FlushIfDirty(); err != nil {
				log.Printf("bursty savings: write state file %s failed: %v", m.file, err)
			}
		case <-m.stop:
			if err := m.FlushIfDirty(); err != nil {
				log.Printf("bursty savings: write state file %s failed: %v", m.file, err)
			}
			return
		}
	}
}

func (m *savingsMeter) Close() {
	m.stopOnce.Do(func() {
		if m.loopStarted {
			close(m.stop)
			<-m.done
			return
		}
		_ = m.FlushIfDirty()
		close(m.done)
	})
}

func (m *savingsMeter) RecordUnknownUsage() {
	m.mu.Lock()
	m.usageUnknownTotal++
	m.mu.Unlock()
}

func (m *savingsMeter) RecordLocalUsage(usage tokenUsage, quote priceQuote) savingsRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.LocalPromptTokens += usage.PromptTokens
	m.state.LocalCompletionTokens += usage.CompletionTokens
	record := savingsRecord{Priced: quote.Priced, Reference: quote.Reference}
	if quote.Priced {
		record.CostMicro = quote.costMicro(usage)
		m.state.SavedUSDMicro += record.CostMicro
		m.currentSavedMicro.Store(m.state.SavedUSDMicro)
		m.references[quote.Reference]++
	}
	m.dirty = true
	m.generation++
	return record
}

func (m *savingsMeter) RecordCloudUsage(usage tokenUsage, quote priceQuote) savingsRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.CloudPromptTokens += usage.PromptTokens
	m.state.CloudCompletionTokens += usage.CompletionTokens
	record := savingsRecord{Priced: quote.Priced, Reference: quote.Reference}
	if quote.Priced {
		record.CostMicro = quote.costMicro(usage)
		m.state.CloudSpendUSDMicro += record.CostMicro
		day := utcDay(m.now().UTC())
		if m.state.CloudSpendUSDMicroByUTCDay == nil {
			m.state.CloudSpendUSDMicroByUTCDay = map[string]int64{}
		}
		m.state.CloudSpendUSDMicroByUTCDay[day] += record.CostMicro
		pruneCloudSpendDays(m.state.CloudSpendUSDMicroByUTCDay, m.now().UTC())
	} else {
		m.cloudUnpricedUsage.PromptTokens += usage.PromptTokens
		m.cloudUnpricedUsage.CompletionTokens += usage.CompletionTokens
	}
	m.dirty = true
	m.generation++
	return record
}

func (m *savingsMeter) SavedUSDHeader() string {
	return formatUSDMicro(m.currentSavedMicro.Load())
}

func (m *savingsMeter) TodayCloudSpendMicro(now time.Time) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.CloudSpendUSDMicroByUTCDay[utcDay(now.UTC())]
}

func (m *savingsMeter) BudgetExhausted(capMicro int64, now time.Time) bool {
	return capMicro > 0 && m.TodayCloudSpendMicro(now) >= capMicro
}

func (m *savingsMeter) Snapshot(localShare float64) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	refs := make(map[string]int64, len(m.references))
	for key, value := range m.references {
		refs[key] = value
	}
	return map[string]any{
		"saved_usd":               microToUSD(m.state.SavedUSDMicro),
		"local_tokens_prompt":     m.state.LocalPromptTokens,
		"local_tokens_completion": m.state.LocalCompletionTokens,
		"cloud_spend_usd":         microToUSD(m.state.CloudSpendUSDMicro),
		"cloud_prompt_tokens":     m.state.CloudPromptTokens,
		"cloud_completion_tokens": m.state.CloudCompletionTokens,
		"usage_unknown_total":     m.usageUnknownTotal,
		"references":              refs,
		"since":                   m.state.Since.UTC().Format(time.RFC3339),
		"local_share":             localShare,
		"cloud_usage_unpriced": map[string]int64{
			"prompt_tokens":     m.cloudUnpricedUsage.PromptTokens,
			"completion_tokens": m.cloudUnpricedUsage.CompletionTokens,
		},
	}
}

func (m *savingsMeter) Totals() (savedMicro int64, cloudSpendMicro int64, topReference string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.SavedUSDMicro, m.state.CloudSpendUSDMicro, topReferenceLocked(m.references)
}

func (m *savingsMeter) HasHistory() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.SavedUSDMicro != 0 ||
		m.state.LocalPromptTokens != 0 ||
		m.state.LocalCompletionTokens != 0 ||
		m.state.CloudSpendUSDMicro != 0 ||
		m.state.CloudPromptTokens != 0 ||
		m.state.CloudCompletionTokens != 0 ||
		len(m.state.CloudSpendUSDMicroByUTCDay) != 0
}

func (m *savingsMeter) FlushIfDirty() error {
	if m.file == "" {
		return nil
	}
	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return nil
	}
	state := m.state
	generation := m.generation
	state.CloudSpendUSDMicroByUTCDay = cloneDaySpend(state.CloudSpendUSDMicroByUTCDay)
	pruneCloudSpendDays(state.CloudSpendUSDMicroByUTCDay, m.now().UTC())
	m.mu.Unlock()

	if err := writeSavingsStateAtomic(m.file, state); err != nil {
		return err
	}
	m.mu.Lock()
	if m.generation == generation {
		m.dirty = false
	}
	m.mu.Unlock()
	return nil
}

func writeSavingsStateAtomic(file string, state persistedSavingsState) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	writeErr := encoder.Encode(state)
	closeErr := tmp.Close()
	if writeErr != nil {
		_ = os.Remove(tmpName)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpName)
		return closeErr
	}
	if err := os.Rename(tmpName, file); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func cloneDaySpend(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func pruneCloudSpendDays(days map[string]int64, now time.Time) {
	if len(days) == 0 {
		return
	}
	cutoff := now.UTC().AddDate(0, 0, -(savingsDayRetention - 1))
	for day := range days {
		parsed, err := time.Parse("2006-01-02", day)
		if err != nil || parsed.Before(time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)) {
			delete(days, day)
		}
	}
}

func utcDay(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func retryAfterUTCMidnight(now time.Time) int64 {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	d := next.Sub(now)
	if d <= 0 {
		return 1
	}
	seconds := int64(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}

func topReferenceLocked(refs map[string]int64) string {
	if len(refs) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	best := keys[0]
	for _, key := range keys[1:] {
		if refs[key] > refs[best] {
			best = key
		}
	}
	return best
}

func microToUSD(value int64) float64 {
	return float64(value) / 1_000_000
}

func formatUSDMicro(value int64) string {
	return strconv.FormatFloat(float64(value)/1_000_000, 'f', 6, 64)
}

func formatUSDLog(value int64) string {
	return fmt.Sprintf("%.2f", float64(value)/1_000_000)
}

func formatUSDBurst(value int64) string {
	return fmt.Sprintf("%.4f", float64(value)/1_000_000)
}
