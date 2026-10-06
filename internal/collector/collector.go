// Package collector 는 스케줄러가 뱉은 결과를 받아 DB에 쌓는다.
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/store"
)

const (
	defaultBatchSize     = 50
	defaultFlushInterval = 2 * time.Second
	finalFlushTimeout    = 5 * time.Second
)

// Collector 는 결과를 모아 배치로 저장한다.
type Collector struct {
	store *store.Store
	ids   map[string]int64 // 모니터 이름 → DB id
	log   *slog.Logger

	batchSize     int
	flushInterval time.Duration

	// Stats() 가 다른 goroutine(/metrics 핸들러)에서 불리므로 atomic 이다.
	written atomic.Int64
	dropped atomic.Int64 // DB에 없는 모니터라 버린 결과
	failed  atomic.Int64 // 저장 실패한 배치의 행 수
}

// Option 은 Collector 설정을 바꾼다.
type Option func(*Collector)

// WithBatchSize 는 몇 개가 쌓이면 저장할지 정한다.
func WithBatchSize(n int) Option {
	return func(c *Collector) {
		if n > 0 {
			c.batchSize = n
		}
	}
}

// WithFlushInterval 은 배치가 안 차도 이 시간마다 저장하게 한다.
func WithFlushInterval(d time.Duration) Option {
	return func(c *Collector) {
		if d > 0 {
			c.flushInterval = d
		}
	}
}

// New 는 Collector 를 만든다. ids 는 SyncMonitors 가 준 이름→id 맵이다.
func New(s *store.Store, ids map[string]int64, log *slog.Logger, opts ...Option) *Collector {
	if log == nil {
		log = slog.Default()
	}
	c := &Collector{
		store:         s,
		ids:           ids,
		log:           log,
		batchSize:     defaultBatchSize,
		flushInterval: defaultFlushInterval,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Stats 는 지금까지 저장/실패한 행 수다.
type Stats struct {
	Written int64
	Dropped int64
	Failed  int64
}

// Stats 는 언제든, 어느 goroutine 에서든 부를 수 있다.
func (c *Collector) Stats() Stats {
	return Stats{Written: c.written.Load(), Dropped: c.dropped.Load(), Failed: c.failed.Load()}
}

// Run 은 results 가 닫힐 때까지 결과를 받아 저장한다.
// onResult 는 저장과 별개로 결과 하나마다 불린다. nil 이면 건너뛴다.
func (c *Collector) Run(ctx context.Context, results <-chan checker.Result, onResult func(checker.Result)) error {
	batch := make([]store.CheckRow, 0, c.batchSize)

	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case res, ok := <-results:
			if !ok {
				// ctx 는 이미 취소됐을 수 있어 새 시한을 판다. 없으면 마지막 배치가 날아간다.
				flushCtx, cancel := context.WithTimeout(context.Background(), finalFlushTimeout)
				defer cancel()
				c.flush(flushCtx, batch)
				return nil
			}

			if onResult != nil {
				onResult(res)
			}

			row, err := c.toRow(res)
			if err != nil {
				c.dropped.Add(1)
				c.log.Warn("결과를 저장할 수 없다", "monitor", res.Monitor, "err", err)
				continue
			}
			batch = append(batch, row)

			if len(batch) >= c.batchSize {
				c.flush(ctx, batch)
				batch = batch[:0]
			}

		case <-ticker.C:
			if len(batch) > 0 {
				c.flush(ctx, batch)
				batch = batch[:0]
			}
		}
	}
}

// flush 는 배치를 저장한다. 실패해도 모니터링은 계속돼야 하므로 로그만 남긴다.
func (c *Collector) flush(ctx context.Context, batch []store.CheckRow) {
	if len(batch) == 0 {
		return
	}
	if err := c.store.InsertChecks(ctx, batch); err != nil {
		c.failed.Add(int64(len(batch)))
		c.log.Error("체크 저장 실패", "rows", len(batch), "err", err)
		return
	}
	c.written.Add(int64(len(batch)))
}

// toRow 는 체크 결과를 DB 행으로 바꾼다.
func (c *Collector) toRow(res checker.Result) (store.CheckRow, error) {
	id, ok := c.ids[res.Monitor]
	if !ok {
		return store.CheckRow{}, fmt.Errorf("DB에 등록되지 않은 모니터")
	}

	row := store.CheckRow{
		MonitorID:  id,
		Type:       res.Type,
		CheckedAt:  res.CheckedAt,
		OK:         res.OK,
		StatusCode: res.StatusCode,
		Warning:    res.Warning,
		LatencyMS:  res.Latency.Milliseconds(),
	}
	if res.Err != nil {
		row.Error = res.Err.Error()
	}
	return row, nil
}
