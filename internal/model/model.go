// Package model holds atelier's pure domain types, enums and validation.
package model

import (
	"fmt"
	"time"
)

// Status is the lifecycle status of a task.
type Status string

// Task statuses.
const (
	StatusBacklog   Status = "backlog"
	StatusActive    Status = "active"
	StatusReview    Status = "review"
	StatusDone      Status = "done"
	StatusCancelled Status = "cancelled"
)

// AllStatuses lists every status in display order (TUI keys 1-5).
var AllStatuses = []Status{StatusBacklog, StatusActive, StatusReview, StatusDone, StatusCancelled}

// ParseStatus validates s and returns it as a Status.
func ParseStatus(s string) (Status, error) {
	for _, st := range AllStatuses {
		if string(st) == s {
			return st, nil
		}
	}
	return "", fmt.Errorf("invalid status %q (want one of backlog, active, review, done, cancelled)", s)
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	_, err := ParseStatus(string(s))
	return err == nil
}

// Open reports whether the task is still in progress (backlog, active or review).
func (s Status) Open() bool {
	switch s {
	case StatusBacklog, StatusActive, StatusReview:
		return true
	}
	return false
}

// Mode is the access mode of a repo within a task.
type Mode string

// Repo modes.
const (
	ModeRW Mode = "rw"
	ModeRO Mode = "ro"
)

// ParseMode validates s and returns it as a Mode.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeRW, ModeRO:
		return Mode(s), nil
	}
	return "", fmt.Errorf("invalid mode %q (want rw or ro)", s)
}

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	_, err := ParseMode(string(m))
	return err == nil
}

// SessionState is the state of a Claude session.
type SessionState string

// Session states.
const (
	StateRunning SessionState = "running"
	StateWaiting SessionState = "waiting"
	StateIdle    SessionState = "idle"
	StateEnded   SessionState = "ended"
	StateStopped SessionState = "stopped"
)

// AllSessionStates lists every session state.
var AllSessionStates = []SessionState{StateRunning, StateWaiting, StateIdle, StateEnded, StateStopped}

// ParseSessionState validates s and returns it as a SessionState.
func ParseSessionState(s string) (SessionState, error) {
	for _, st := range AllSessionStates {
		if string(st) == s {
			return st, nil
		}
	}
	return "", fmt.Errorf("invalid session state %q (want one of running, waiting, idle, ended, stopped)", s)
}

// Valid reports whether s is a known session state.
func (s SessionState) Valid() bool {
	_, err := ParseSessionState(string(s))
	return err == nil
}

// Live reports whether the session is still alive (running, waiting or idle).
func (s SessionState) Live() bool {
	switch s {
	case StateRunning, StateWaiting, StateIdle:
		return true
	}
	return false
}

// TaskState returns the state that stands for a task's sessions, as its
// list dot shows it: the highest of waiting > running > idle > stopped, or
// "" when there are none of those (no sessions, or all ended).
func TaskState(ss []Session) SessionState {
	rank := func(st SessionState) int {
		switch st {
		case StateWaiting:
			return 4
		case StateRunning:
			return 3
		case StateIdle:
			return 2
		case StateStopped:
			return 1
		}
		return 0
	}
	var best SessionState
	for _, s := range ss {
		if rank(s.State) > rank(best) {
			best = s.State
		}
	}
	return best
}

// Task is a unit of work spanning one or more repos.
type Task struct {
	ID          int64
	Slug        string // unique, [a-z0-9-], ≤40 chars
	Title       string
	Description string
	Status      Status
	Project     string // "" allowed
	Branch      string // "atl/" + Slug
	PortOffset  int    // 0,100,200...
	Base        string // base branch choice for rw repos: BaseAuto, BaseDefault or a branch name
	Repos       []TaskRepo
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Task base choices (Task.Base). Any other value is the exact name of a
// branch that must exist on origin (refs/remotes/origin/<name>) in every rw
// repo of the task.
const (
	// BaseAuto: each rw repo branches from its integration branch
	// (origin/dev, else origin/develop, else origin's default branch).
	BaseAuto = ""
	// BaseDefault: each rw repo branches from origin's default branch
	// (production patches).
	BaseDefault = ":default"
)

// TaskRepo is a repo attached to a task.
type TaskRepo struct {
	Name       string // dir name under $ATL_REPOS
	Mode       Mode
	BaseBranch string // origin branch the task branch was created from, e.g. "dev" or "main" ("" for ro)
	BaseCommit string // sha of origin/<BaseBranch> the task branch was created from ("" for ro)
}

// Session is a Claude Code session running inside a task.
type Session struct {
	ID              int64
	TaskID          int64
	ClaudeSessionID string // "" until SessionStart hook fires
	TmuxTarget      string // tmux window id, e.g. "@12" ("" if none)
	Name            string // tmux window name, e.g. "claude-3"
	State           SessionState
	CreatedAt       time.Time
	LastEventAt     time.Time
	TranscriptPath  string // Claude Code transcript JSONL ("" until a hook reports it)
}

// Note authors.
const (
	AuthorUser      = "user"
	AuthorAssistant = "assistant"
)

// ValidAuthor reports whether a is a known note author.
func ValidAuthor(a string) bool { return a == AuthorUser || a == AuthorAssistant }
