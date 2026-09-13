// Package collector 는 스케줄러가 뱉은 결과를 받아 DB에 쌓는다.
//
// 스펙 5절 다이어그램의 collector 자리다.
//
//	scheduler → worker × N → resultCh → [collector] → DB
//
// 결과 채널을 읽는 곳이 여기 하나뿐이라, SQLite 쓰기가 자연스럽게
// 한 goroutine 으로 직렬화된다 (스펙 7절 함정: SQLite 동시 쓰기 잠금).
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Archibald1948/upcheck/internal/checker"
	"github.com/Archibald1948/upcheck/internal/store"
)

// 기본값.
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

	// 통계
	written int64
	dropped int64 // DB에 없는 모니터라 버린 결과
	failed  int64 // 저장 실패한 배치의 행 수
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

func (c *Collector) Stats() Stats {
	// Run 이 끝난 뒤에 부르는 걸 전제한다. Run 이 도는 동안 부르면
	// 값이 찢어질 수 있으므로, 실시간으로 봐야 하면 atomic 으로 바꿔야 한다.
	return Stats{Written: c.written, Dropped: c.dropped, Failed: c.failed}
}

// Run 은 results 가 닫힐 때까지 결과를 받아 저장한다.
//
// onResult 는 저장과 별개로 결과 하나마다 불린다(콘솔 출력 등). nil 이면 건너뛴다.
// M3에서 상태 전이 판정과 알림이 여기에 붙는다.
//
// results 채널이 닫히는 것이 곧 "스케줄러가 완전히 정리됐다"는 신호다.
// 그래서 ctx.Done() 을 따로 보지 않고 채널만 끝까지 읽는다.
// 중간에 빠져나가면 이미 나온 결과를 잃는다.
func (c *Collector) Run(ctx context.Context, results <-chan checker.Result, onResult func(checker.Result)) error {
	batch := make([]store.CheckRow, 0, c.batchSize)

	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case res, ok := <-results:
			if !ok {
				// 채널이 닫혔다 = 종료. 남은 걸 마저 저장하고 끝낸다.
				//
				// ctx 는 이미 취소됐을 가능성이 높다(Ctrl+C 로 여기까지 온 경우).
				// 취소된 ctx 로는 질의가 즉시 실패하므로 새 시한을 판다.
				// 이게 없으면 마지막 배치가 통째로 날아간다.
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
				c.dropped++
				c.log.Warn("결과를 저장할 수 없다", "monitor", res.Monitor, "err", err)
				continue
			}
			batch = append(batch, row)

			if len(batch) >= c.batchSize {
				c.flush(ctx, batch)
				batch = batch[:0] // 길이만 0으로, 확보한 메모리는 재사용
			}

		case <-ticker.C:
			// 배치가 안 차도 주기적으로 비운다.
			// 이게 없으면 체크가 뜸한 설정에서 결과가 한참 메모리에만 있는다.
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
		c.failed += int64(len(batch))
		c.log.Error("체크 저장 실패", "rows", len(batch), "err", err)
		return
	}
	c.written += int64(len(batch))
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
		// Duration 을 밀리초 정수로 접는다. 마이크로초 이하는 버린다 —
		// 네트워크 응답시간에서 그 정밀도는 의미가 없고, 저장 공간만 먹는다.
		LatencyMS: res.Latency.Milliseconds(),
	}
	if res.Err != nil {
		row.Error = res.Err.Error()
	}
	return row, nil
}
