package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

var columns = []string{"todo", "doing", "done"}

var columnTitles = map[string]string{"todo": "To do", "doing": "In progress", "done": "Done"}

func validColumn(c string) bool {
	_, ok := columnTitles[c]
	return ok
}

// Task is one card on the board.
type Task struct {
	ID     int
	Title  string
	Column string
	By     string
}

// Board is the canonical server-side state. Every change bumps Version.
type Board struct {
	mu       sync.Mutex
	version  uint64
	nextID   int
	tasks    map[int]*Task
	activity []string // newest first
}

const activityLimit = 20

func NewBoard() *Board {
	return &Board{tasks: make(map[int]*Task), nextID: 1}
}

// Add creates a task in "todo" and returns the new version.
func (b *Board) Add(title, by string) (task Task, version uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := &Task{ID: b.nextID, Title: title, Column: "todo", By: by}
	b.nextID++
	b.tasks[t.ID] = t
	b.record(fmt.Sprintf("%s added %q", by, title))
	b.version++
	return *t, b.version
}

// Move puts a task in another column. It returns the task's previous
// column, or false if the task does not exist.
func (b *Board) Move(id int, to, by string) (from string, version uint64, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.tasks[id]
	if !ok {
		return "", b.version, false
	}
	from, t.Column = t.Column, to
	b.record(fmt.Sprintf("%s moved %q to %s", by, t.Title, columnTitles[to]))
	b.version++
	return from, b.version, true
}

func (b *Board) record(entry string) {
	entry = time.Now().Format("15:04:05") + "  " + entry
	b.activity = append([]string{entry}, b.activity...)
	if len(b.activity) > activityLimit {
		b.activity = b.activity[:activityLimit]
	}
}

// Column returns the tasks in a column, oldest first.
func (b *Board) Column(c string) []Task {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Task
	for _, t := range b.tasks {
		if t.Column == c {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (b *Board) Activity() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.activity...)
}

func (b *Board) Version() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version
}
