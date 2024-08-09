package concurrency

import (
	"testing"
	"time"
)

type results []int

func TestWorkerPool(t *testing.T) {
	cases := []struct {
		name    string
		workers int
		tasks   int
	}{
		{
			name:    "1 task, 2 workers",
			workers: 2,
			tasks:   1,
		},
		{
			name:    "2 tasks, 1 worker",
			workers: 1,
			tasks:   2,
		},
		{
			name:    "2 tasks, 2 workers",
			workers: 2,
			tasks:   2,
		},
		{
			name:    "0 tasks, 2 workers",
			workers: 2,
			tasks:   0,
		},
		{
			name:    "6 tasks, 3 workers",
			workers: 3,
			tasks:   6,
		},
		{
			name:    "6 tasks, 4 workers",
			workers: 4,
			tasks:   6,
		},
	}

	for _, ccase := range cases {
		pool := NewWorkerPool(ccase.workers)

		results := make(results, ccase.tasks)
		for i := 0; i < ccase.tasks; i++ {
			target := &results[i]
			pool.RegisterTask(func() { *target += 1 })
		}

		pool.Execute()
		for _, v := range results {
			if v != 1 {
				t.Errorf("[%s] expected array of ones, but got %v", ccase.name, results)
			}
		}
	}
}

func TestTiming(t *testing.T) {
	cases := []struct {
		name      string
		workers   int
		tasks     int
		endBefore time.Duration
	}{
		{
			name:      "5 tasks, 1 worker",
			workers:   1,
			tasks:     5,
			endBefore: 7 * time.Second,
		},
		{
			name:      "5 tasks, 2 worker",
			workers:   5,
			tasks:     10,
			endBefore: 4 * time.Second,
		},
		{
			name:      "5 tasks, 5 workers",
			workers:   10,
			tasks:     10,
			endBefore: 2 * time.Second,
		},
	}

	for _, ccase := range cases {
		pool := NewWorkerPool(ccase.workers)

		for i := 0; i < ccase.tasks; i++ {
			pool.RegisterTask(func() { time.Sleep(1 * time.Second) })
		}

		now := time.Now()
		pool.Execute()
		elapsed := time.Since(now)

		if elapsed >= ccase.endBefore {
			t.Errorf("[%s] should have terminated before %s", ccase.name, ccase.endBefore.String())
		}
	}
}
