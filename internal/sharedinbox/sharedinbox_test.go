package sharedinbox

import (
	"context"
	"errors"
	"testing"
)

func TestValidStatus(t *testing.T) {
	t.Parallel()
	valid := []string{StatusOpen, StatusInProgress, StatusWaiting, StatusResolved, StatusClosed}
	for _, s := range valid {
		if !validStatus(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	if validStatus("bogus") {
		t.Error("bogus should be invalid")
	}
}

func TestSetStatusRejectsInvalid(t *testing.T) {
	t.Parallel()
	svc := NewService(nil, nil)
	_, err := svc.SetStatus(context.Background(), "t", "inbox", "email", "bogus")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("want ErrInvalidInput, got %v", err)
	}
}

func TestAddNoteValidates(t *testing.T) {
	t.Parallel()
	svc := NewService(nil, nil)
	_, err := svc.AddNote(context.Background(), "t", "inbox", "email", "author", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty note must reject: %v", err)
	}
	_, err = svc.AddNote(context.Background(), "", "inbox", "email", "author", "hi")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("missing tenant must reject: %v", err)
	}
}

func TestAssignEmailValidates(t *testing.T) {
	t.Parallel()
	svc := NewService(nil, nil)
	_, err := svc.AssignEmail(context.Background(), "t", "inbox", "email", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty assignee must reject: %v", err)
	}
}
