package concurrency

import "sync"

type WorkerPool struct {
	workers int
	tasks   []func()
}

func NewWorkerPool(workers int, tasks ...func()) *WorkerPool {
	return &WorkerPool{workers: workers, tasks: tasks}
}

func (wl *WorkerPool) RegisterTask(tasks ...func()) {
	wl.tasks = append(wl.tasks, tasks...)
}

func (wl *WorkerPool) nextTask() func() {
	task := wl.tasks[0]
	wl.tasks = wl.tasks[1:]
	return task
}

func (wl *WorkerPool) Execute() {
	leases := make(chan bool, wl.workers)
	for i := 0; i < wl.workers; i++ {
		leases <- true
	}

	wait := sync.WaitGroup{}
	for len(wl.tasks) > 0 {
		task := wl.nextTask()
		<-leases
		wait.Add(1)

		go func() {
			task()
			leases <- true
			wait.Done()
		}()
	}

	wait.Wait()
}
