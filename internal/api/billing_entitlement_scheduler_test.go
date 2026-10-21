package api

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
)

func TestEntitlementLanesSustainMixedDueWork(t *testing.T) {
	for _, n := range []int{100, 500} {
		for _, replicas := range []int{1, 2} {
			t.Run(fmt.Sprintf("jobs%d_replicas%d", n, replicas), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					type job struct {
						id          uuid.UUID
						due, last   time.Time
						retry, busy bool
						completions int
						maxGap      time.Duration
					}
					began := time.Now()
					jobs := make([]job, n)
					indexes := map[uuid.UUID]int{}
					for i := range jobs {
						jobs[i] = job{id: uuid.New(), due: began, last: began, retry: i%10 == 0}
						indexes[jobs[i].id] = i
					}
					var mu sync.Mutex
					var active, peak, requests, emptyClaims, idleQueries int
					var fastDone time.Time
					ctx, cancel := context.WithCancel(t.Context())
					var wg sync.WaitGroup
					for range replicas {
						for lane, retry := range []bool{false, true} {
							wg.Add(1)
							go func() {
								defer wg.Done()
								runEntitlementLane(ctx, 4, func(ctx context.Context, limit int) ([]entitlementJob, error) {
									mu.Lock()
									defer mu.Unlock()
									result := []entitlementJob{}
									for range limit {
										chosen := -1
										for i := range jobs {
											j := &jobs[i]
											if !j.busy && j.retry == retry && !j.due.After(time.Now()) && (chosen < 0 || j.due.Before(jobs[chosen].due)) {
												chosen = i
											}
										}
										if chosen < 0 {
											break
										}
										j := &jobs[chosen]
										j.busy = true
										result = append(result, entitlementJob{Contract: j.id})
										active++
										peak = max(peak, active)
									}
									if len(result) == 0 {
										emptyClaims++
									}
									return result, nil
								}, func(ctx context.Context, claimed entitlementJob) {
									i := indexes[claimed.Contract]
									cost := 2
									if i%3 == 1 {
										cost = 7
									}
									mu.Lock()
									requests += cost
									mu.Unlock()
									// Synthetic request latency/cost, not a provider capacity measurement.
									latency := time.Duration(cost) * 10 * time.Millisecond
									if i == 1 {
										latency = 5 * time.Second
									}
									time.Sleep(latency)
									mu.Lock()
									defer mu.Unlock()
									j := &jobs[i]
									active--
									j.busy = false
									j.completions++
									j.retry = false
									j.maxGap = max(j.maxGap, time.Since(j.last))
									j.last = time.Now()
									j.due = time.Now().Add(20 * time.Second)
									if i%3 == 2 {
										j.due = time.Now().Add(time.Hour)
									} // Legacy issued work.
									if i == 2 && fastDone.IsZero() {
										fastDone = time.Now()
									}
								}, func(context.Context) time.Duration {
									mu.Lock()
									idleQueries++
									mu.Unlock()
									return entitlementPollDelay(time.Second, lane)
								})
							}()
						}
					}
					time.Sleep(90 * time.Second)
					cancel()
					wg.Wait()
					synctest.Wait()
					if peak > replicas*8 || fastDone.Sub(began) >= time.Second {
						t.Fatal("slow job held other capacity", peak, fastDone.Sub(began))
					}
					var worst time.Duration
					for i, j := range jobs {
						minimum := 3
						if i%3 == 2 {
							minimum = 1
						}
						worst = max(worst, j.maxGap)
						if j.completions < minimum || j.busy || j.maxGap > 30*time.Second {
							t.Fatalf("job%d missed modeled freshness: %+v", i, j)
						}
					}
					t.Logf("synthetic90s jobs=%d replicas=%d peak=%d provider_requests=%d max_gap=%s empty_claims=%d idle_queries=%d", n, replicas, peak, requests, worst, emptyClaims, idleQueries)
				})
			})
		}
	}
}

func TestEntitlementLaneIdlePollingDoesNotScaleWithWorkers(t *testing.T) {
	for _, workers := range []int{4, 16} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				claims, idle := 0, 0
				done := make(chan struct{})
				go func() {
					defer close(done)
					runEntitlementLane(ctx, workers, func(context.Context, int) ([]entitlementJob, error) { claims++; return nil, nil }, func(context.Context, entitlementJob) { t.Error("empty queue ran job") }, func(context.Context) time.Duration { idle++; return time.Second })
				}()
				time.Sleep(10 * time.Second)
				cancel()
				<-done
				if claims < 10 || claims > 11 || idle != claims {
					t.Fatal("idle polling multiplied or spun", workers, claims, idle)
				}
			})
		})
	}
}

func TestEntitlementLaneBacksOffClaimErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		began := time.Now()
		runEntitlementLane(ctx, 4, func(context.Context, int) ([]entitlementJob, error) {
			calls++
			if calls == 3 {
				cancel()
			}
			return nil, fmt.Errorf("database unavailable")
		}, func(context.Context, entitlementJob) { t.Error("failed claim ran job") }, func(context.Context) time.Duration { t.Fatal("error used idle polling"); return 0 })
		if calls != 3 || time.Since(began) != 2*time.Second {
			t.Fatal("claim error spun without backoff", calls, time.Since(began))
		}
	})
}
