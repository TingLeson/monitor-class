package classroom

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// The classroom lifecycle's runtime half (§47/§48/§49) against a fake runtime layer.
//
// WHAT these tests are about: the ORDER and the consequence. Opening a classroom must
// announce the run to its students; closing it must first end the run's sessions and then
// announce the end; and NEITHER may fail the teacher's request, because the state change
// is already committed when they run (§33).

// call records one runtime hook invocation, in order.
type call struct {
	name string
	id   uuid.UUID
}

type fakeRuntime struct {
	calls []call
	// closedSessions is what CloseRunSessions reports.
	closedSessions int
	// failures makes every hook fail, which the service must absorb.
	closeErr  error
	openedErr error
	closedErr error
}

func (f *fakeRuntime) CloseRunSessions(_ context.Context, runID uuid.UUID) (int, error) {
	f.calls = append(f.calls, call{name: "close_run_sessions", id: runID})
	if f.closeErr != nil {
		return 0, f.closeErr
	}
	return f.closedSessions, nil
}

func (f *fakeRuntime) RoomOpened(_ context.Context, classroomID, runID uuid.UUID) error {
	f.calls = append(f.calls, call{name: "room_opened", id: runID})
	return f.openedErr
}

func (f *fakeRuntime) RoomClosed(_ context.Context, classroomID, runID uuid.UUID) error {
	f.calls = append(f.calls, call{name: "room_closed", id: runID})
	return f.closedErr
}

func (f *fakeRuntime) names() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.name)
	}
	return out
}

func TestOpenAnnouncesTheNewRun(t *testing.T) {
	h := newHarness(t)
	runtime := &fakeRuntime{}
	h.service.WithRuntimeHooks(runtime)

	created := h.mustCreate(t, "C++ 晚自习")
	_, run, err := h.service.Open(context.Background(), created.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}

	if got := runtime.names(); len(got) != 1 || got[0] != "room_opened" {
		t.Fatalf("runtime calls = %v, want [room_opened]", got)
	}
	if runtime.calls[0].id != run.ID {
		t.Fatalf("announced run = %s, want %s", runtime.calls[0].id, run.ID)
	}
}

func TestCloseEndsTheRunsSessionsBeforeAnnouncingIt(t *testing.T) {
	h := newHarness(t)
	runtime := &fakeRuntime{closedSessions: 3}
	h.service.WithRuntimeHooks(runtime)

	created := h.mustCreate(t, "C++ 晚自习")
	_, run, err := h.service.Open(context.Background(), created.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	runtime.calls = nil

	if _, _, err := h.service.Close(context.Background(), created.ID, h.teacher.ID); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	// §49's order: the control plane's rows first (the sessions are over), then the
	// clients. Reversing it would tell a student "the lesson is over" while their session
	// row still said ONLINE.
	want := []string{"close_run_sessions", "room_closed"}
	got := runtime.names()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("runtime calls = %v, want %v", got, want)
	}
	if runtime.calls[0].id != run.ID {
		t.Fatalf("closed sessions of run %s, want %s", runtime.calls[0].id, run.ID)
	}
}

func TestARuntimeFailureNeverFailsTheTeachersRequest(t *testing.T) {
	h := newHarness(t)
	runtime := &fakeRuntime{
		closeErr:  errors.New("database is down"),
		openedErr: errors.New("no audience"),
		closedErr: errors.New("no audience"),
	}
	h.service.WithRuntimeHooks(runtime)

	created := h.mustCreate(t, "C++ 晚自习")
	opened, run, err := h.service.Open(context.Background(), created.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Open() must succeed even when the announcement fails: %v", err)
	}
	if opened.Status != StatusOpen || run == nil {
		t.Fatalf("open result = %+v / %+v", opened, run)
	}

	closed, _, err := h.service.Close(context.Background(), created.ID, h.teacher.ID)
	if err != nil {
		t.Fatalf("Close() must succeed even when the runtime layer fails: %v", err)
	}
	if closed.Status != StatusClosed {
		t.Fatalf("status = %s, want CLOSED: the database is the source of truth (§33)", closed.Status)
	}
}

func TestLifecycleWithoutARuntimeLayerStillWorks(t *testing.T) {
	h := newHarness(t)
	created := h.mustCreate(t, "C++ 晚自习")

	if _, _, err := h.service.Open(context.Background(), created.ID, h.teacher.ID); err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if _, _, err := h.service.Close(context.Background(), created.ID, h.teacher.ID); err != nil {
		t.Fatalf("Close(): %v", err)
	}
}
