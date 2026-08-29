package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadRESPVariants(t *testing.T) {
	input := "+PONG\r\n:7\r\n$5\r\nhello\r\n*2\r\n$3\r\none\r\n:2\r\n"
	reader := bufio.NewReader(strings.NewReader(input))

	value, err := readRESP(reader)
	if err != nil || value != "PONG" {
		t.Fatalf("simple string: %#v, %v", value, err)
	}
	value, err = readRESP(reader)
	if err != nil || value != int64(7) {
		t.Fatalf("integer: %#v, %v", value, err)
	}
	value, err = readRESP(reader)
	if err != nil || string(rawBytes(value)) != "hello" {
		t.Fatalf("bulk string: %#v, %v", value, err)
	}
	value, err = readRESP(reader)
	items, ok := value.([]any)
	if err != nil || !ok || len(items) != 2 || string(rawBytes(items[0])) != "one" || items[1] != int64(2) {
		t.Fatalf("array: %#v, %v", value, err)
	}
}

func TestRedisPingHasTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		close(accepted)
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()

	queue := NewRedis(listener.Addr().String(), "", 0, "test")
	started := time.Now()
	err = queue.Ping()
	if err == nil {
		t.Fatal("expected ping to fail")
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("ping did not connect")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("ping exceeded timeout: %s", elapsed)
	}
}

func TestRedisQueueLifecycleAndRunningRecovery(t *testing.T) {
	redis := newFakeRedis(t)
	queue := NewRedis(redis.addr(), "", 0, "test")

	now := time.Now().Unix()
	recovered := Task{ID: "task-recovered", Kind: "echo", Status: "running", Payload: map[string]any{"value": "recovered"}, CreatedAt: now, UpdatedAt: now}
	raw, err := json.Marshal(recovered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.command(context.Background(), "SET", queue.taskKey(recovered.ID), string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.command(context.Background(), "SADD", queue.indexKey(), recovered.ID); err != nil {
		t.Fatal(err)
	}

	queue.Register("echo", func(task *Task) (map[string]any, error) {
		return map[string]any{"value": task.Payload["value"]}, nil
	})
	queue.Start(1)
	item := queue.Submit("echo", map[string]any{"value": "new"})

	waitForRedisTask(t, queue, recovered.ID, "completed")
	completed := waitForRedisTask(t, queue, item.ID, "completed")
	if completed.Result["value"] != "new" {
		t.Fatalf("unexpected result: %#v", completed.Result)
	}
}

func waitForRedisTask(t *testing.T, queue *RedisQueue, id, status string) Task {
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

type fakeRedis struct {
	listener net.Listener
	mu       sync.Mutex
	values   map[string]string
	sets     map[string]map[string]bool
	lists    map[string][]string
	wakeup   chan struct{}
}

func newFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeRedis{listener: listener, values: map[string]string{}, sets: map[string]map[string]bool{}, lists: map[string][]string{}, wakeup: make(chan struct{})}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func (s *fakeRedis) addr() string { return s.listener.Addr().String() }

func (s *fakeRedis) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeRedis) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	args, err := readRedisCommand(reader)
	if err != nil || len(args) == 0 {
		return
	}
	command := strings.ToUpper(args[0])
	switch command {
	case "AUTH", "SELECT":
		writeRESP(conn, "+OK\r\n")
	case "PING":
		writeRESP(conn, "+PONG\r\n")
	case "SET":
		if len(args) < 3 {
			writeRESP(conn, "-ERR wrong number of arguments\r\n")
			return
		}
		s.mu.Lock()
		s.values[args[1]] = args[2]
		s.mu.Unlock()
		writeRESP(conn, "+OK\r\n")
	case "GET":
		s.mu.Lock()
		value, ok := s.values[args[1]]
		s.mu.Unlock()
		if !ok {
			writeRESP(conn, "$-1\r\n")
			return
		}
		writeRESP(conn, fmt.Sprintf("$%d\r\n%s\r\n", len(value), value))
	case "SADD":
		s.mu.Lock()
		if s.sets[args[1]] == nil {
			s.sets[args[1]] = map[string]bool{}
		}
		added := 0
		for _, member := range args[2:] {
			if !s.sets[args[1]][member] {
				s.sets[args[1]][member] = true
				added++
			}
		}
		s.mu.Unlock()
		writeRESP(conn, ":"+strconv.Itoa(added)+"\r\n")
	case "SMEMBERS":
		s.mu.Lock()
		members := make([]string, 0, len(s.sets[args[1]]))
		for member := range s.sets[args[1]] {
			members = append(members, member)
		}
		s.mu.Unlock()
		var builder strings.Builder
		builder.WriteString("*" + strconv.Itoa(len(members)) + "\r\n")
		for _, member := range members {
			builder.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(member), member))
		}
		writeRESP(conn, builder.String())
	case "RPUSH":
		s.mu.Lock()
		s.lists[args[1]] = append(s.lists[args[1]], args[2:]...)
		oldWakeup := s.wakeup
		s.wakeup = make(chan struct{})
		s.mu.Unlock()
		close(oldWakeup)
		writeRESP(conn, ":1\r\n")
	case "BLPOP":
		if len(args) < 3 {
			writeRESP(conn, "-ERR wrong number of arguments\r\n")
			return
		}
		timeout, _ := strconv.Atoi(args[len(args)-1])
		key := args[1]
		for {
			s.mu.Lock()
			if len(s.lists[key]) > 0 {
				value := s.lists[key][0]
				s.lists[key] = s.lists[key][1:]
				s.mu.Unlock()
				writeRESP(conn, fmt.Sprintf("*2\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(key), key, len(value), value))
				return
			}
			wakeup := s.wakeup
			s.mu.Unlock()
			select {
			case <-wakeup:
			case <-time.After(time.Duration(timeout) * time.Second):
				writeRESP(conn, "*-1\r\n")
				return
			}
		}
	default:
		writeRESP(conn, "-ERR unsupported command\r\n")
	}
}

func readRedisCommand(reader *bufio.Reader) ([]string, error) {
	prefix, err := reader.ReadByte()
	if err != nil || prefix != '*' {
		return nil, errors.New("invalid command")
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return nil, err
	}
	args := make([]string, count)
	for i := range args {
		prefix, err = reader.ReadByte()
		if err != nil || prefix != '$' {
			return nil, errors.New("invalid bulk argument")
		}
		line, err = reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		length, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			return nil, err
		}
		data := make([]byte, length+2)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:length])
	}
	return args, nil
}

func writeRESP(writer io.Writer, value string) {
	_, _ = io.WriteString(writer, value)
}

var _ = bytes.NewBuffer
