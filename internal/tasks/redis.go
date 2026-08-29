package tasks

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RedisQueue is a dependency-free Redis task backend. It uses only the small
// RESP subset needed by the task lifecycle, so deployments without the Go
// module cache can still build the service.
type RedisQueue struct {
	addr     string
	password string
	database int
	prefix   string
	mu       sync.Mutex
	handlers map[string]func(*Task) (map[string]any, error)
}

func NewRedis(addr, password string, database int, prefix string) *RedisQueue {
	if strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:6379"
	}
	if database < 0 {
		database = 0
	}
	if strings.TrimSpace(prefix) == "" {
		prefix = "gptgrok2api"
	}
	return &RedisQueue{addr: addr, password: password, database: database, prefix: prefix, handlers: map[string]func(*Task) (map[string]any, error){}}
}

func (q *RedisQueue) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := q.command(ctx, "PING")
	return err
}

func (q *RedisQueue) Register(kind string, handler func(*Task) (map[string]any, error)) {
	q.mu.Lock()
	q.handlers[kind] = handler
	q.mu.Unlock()
}

func (q *RedisQueue) Start(workers int) {
	if workers < 1 {
		workers = 1
	}
	q.recoverRunning()
	for i := 0; i < workers; i++ {
		go q.worker()
	}
}

// recoverRunning returns tasks whose worker disappeared before it could write
// a terminal state back to Redis. BLPOP removes a task from the queue, so a
// running task must be explicitly put back before workers start.
func (q *RedisQueue) recoverRunning() {
	raw, err := q.command(context.Background(), "SMEMBERS", q.indexKey())
	if err != nil {
		return
	}
	items, ok := raw.([]any)
	if !ok {
		return
	}
	for _, value := range items {
		id := stringValue(rawBytes(value))
		if id == "" {
			continue
		}
		task, ok := q.Get(id)
		if !ok || task.Status != "running" {
			continue
		}
		task.Status = "queued"
		task.UpdatedAt = time.Now().Unix()
		_ = q.save(&task)
	}
}

func (q *RedisQueue) Submit(kind string, payload map[string]any) *Task {
	now := time.Now().Unix()
	task := &Task{ID: taskID(), Kind: kind, Status: "queued", Payload: payload, CreatedAt: now, UpdatedAt: now}
	_ = q.save(task)
	return clone(task)
}

func (q *RedisQueue) Get(id string) (Task, bool) {
	raw, err := q.command(context.Background(), "GET", q.taskKey(id))
	if err != nil || raw == nil {
		return Task{}, false
	}
	var task Task
	if json.Unmarshal(rawBytes(raw), &task) != nil {
		return Task{}, false
	}
	return *clone(&task), true
}

func (q *RedisQueue) Cancel(id string) bool {
	task, ok := q.Get(id)
	if !ok || task.Status == "completed" || task.Status == "failed" || task.Status == "cancelled" {
		return false
	}
	task.Status = "cancelled"
	task.UpdatedAt = time.Now().Unix()
	return q.save(&task) == nil
}

func (q *RedisQueue) List() []Task {
	raw, err := q.command(context.Background(), "SMEMBERS", q.indexKey())
	if err != nil {
		return []Task{}
	}
	items, _ := raw.([]any)
	result := make([]Task, 0, len(items))
	for _, value := range items {
		if task, ok := q.Get(stringValue(rawBytes(value))); ok {
			result = append(result, task)
		}
	}
	return result
}

func (q *RedisQueue) worker() {
	for {
		raw, err := q.command(context.Background(), "BLPOP", q.queueKey(), "5")
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		values, ok := raw.([]any)
		if !ok || len(values) < 2 {
			continue
		}
		id := stringValue(rawBytes(values[1]))
		task, ok := q.Get(id)
		if !ok || task.Status != "queued" {
			continue
		}
		task.Status = "running"
		task.UpdatedAt = time.Now().Unix()
		_ = q.save(&task)
		q.mu.Lock()
		handler := q.handlers[task.Kind]
		q.mu.Unlock()
		var result map[string]any
		var handlerErr error
		if handler == nil {
			handlerErr = errors.New("no task handler")
		} else {
			result, handlerErr = handler(&task)
		}
		current, exists := q.Get(id)
		if !exists || current.Status == "cancelled" {
			continue
		}
		current.UpdatedAt = time.Now().Unix()
		if handlerErr != nil {
			current.Status = "failed"
			current.Error = handlerErr.Error()
		} else {
			current.Status = "completed"
			current.Progress = 100
			current.Result = result
		}
		_ = q.save(&current)
	}
}

func (q *RedisQueue) save(task *Task) error {
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	if _, err := q.command(context.Background(), "SET", q.taskKey(task.ID), string(raw), "EX", "604800"); err != nil {
		return err
	}
	_, _ = q.command(context.Background(), "SADD", q.indexKey(), task.ID)
	if task.Status == "queued" {
		_, err = q.command(context.Background(), "RPUSH", q.queueKey(), task.ID)
	}
	return err
}

func (q *RedisQueue) taskKey(id string) string { return q.prefix + ":task:" + id }
func (q *RedisQueue) queueKey() string         { return q.prefix + ":tasks:queue" }
func (q *RedisQueue) indexKey() string         { return q.prefix + ":tasks:index" }

func (q *RedisQueue) command(ctx context.Context, args ...string) (any, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", q.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if q.password != "" {
		if _, err := redisRoundTrip(conn, append([]string{"AUTH"}, q.password)); err != nil {
			return nil, err
		}
	}
	if q.database != 0 {
		if _, err := redisRoundTrip(conn, []string{"SELECT", strconv.Itoa(q.database)}); err != nil {
			return nil, err
		}
	}
	return redisRoundTrip(conn, args)
}

func redisRoundTrip(conn net.Conn, args []string) (any, error) {
	var builder strings.Builder
	builder.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, arg := range args {
		builder.WriteString("$" + strconv.Itoa(len(arg)) + "\r\n" + arg + "\r\n")
	}
	if _, err := io.WriteString(conn, builder.String()); err != nil {
		return nil, err
	}
	return readRESP(bufio.NewReader(conn))
}

func readRESP(reader *bufio.Reader) (any, error) {
	prefix, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	switch prefix {
	case '+':
		return line, nil
	case '-':
		return nil, errors.New(line)
	case ':':
		value, _ := strconv.ParseInt(line, 10, 64)
		return value, nil
	case '$':
		length, _ := strconv.Atoi(line)
		if length < 0 {
			return nil, nil
		}
		data := make([]byte, length+2)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		return data[:length], nil
	case '*':
		length, _ := strconv.Atoi(line)
		if length < 0 {
			return nil, nil
		}
		items := make([]any, length)
		for i := range items {
			items[i], err = readRESP(reader)
			if err != nil {
				return nil, err
			}
		}
		return items, nil
	default:
		return nil, fmt.Errorf("unsupported Redis RESP type %q", prefix)
	}
}

func rawBytes(value any) []byte {
	switch typed := value.(type) {
	case []byte:
		return typed
	case string:
		return []byte(typed)
	default:
		return []byte(fmt.Sprint(value))
	}
}

func stringValue(value any) string { return strings.TrimSpace(string(rawBytes(value))) }
