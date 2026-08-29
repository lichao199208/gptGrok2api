package tasks

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func waitForTask(t *testing.T, queue *Queue, id string, status string) Task {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if item, ok := queue.Get(id); ok && item.Status == status {
			return item
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach %s", id, status)
	return Task{}
}

func TestQueueWorkerPersistenceAndFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	queue := New(path)
	queue.Register("ok", func(task *Task) (map[string]any, error) { return map[string]any{"echo": task.Payload["value"]}, nil })
	queue.Register("bad", func(*Task) (map[string]any, error) { return nil, errors.New("handler failed") })
	queue.Start(1)
	ok := queue.Submit("ok", map[string]any{"value": "yes"})
	if item := waitForTask(t, queue, ok.ID, "completed"); item.Result["echo"] != "yes" {
		t.Fatalf("unexpected task result: %#v", item.Result)
	}
	bad := queue.Submit("bad", nil)
	if item := waitForTask(t, queue, bad.ID, "failed"); item.Error != "handler failed" {
		t.Fatalf("unexpected task error: %#v", item)
	}
	reloaded := New(path)
	if item, ok := reloaded.Get(ok.ID); !ok || item.Status != "completed" {
		t.Fatalf("completed task was not persisted: %#v %v", item, ok)
	}
}

func TestQueueCancellationIsTerminal(t *testing.T) {
	queue := New(filepath.Join(t.TempDir(), "tasks.json"))
	started := make(chan struct{})
	release := make(chan struct{})
	queue.Register("wait", func(*Task) (map[string]any, error) { close(started); <-release; return nil, nil })
	queue.Start(1)
	item := queue.Submit("wait", nil)
	<-started
	if !queue.Cancel(item.ID) {
		t.Fatal("cancel failed")
	}
	close(release)
	if got := waitForTask(t, queue, item.ID, "cancelled"); got.Status != "cancelled" {
		t.Fatalf("cancelled task was overwritten: %#v", got)
	}
}
