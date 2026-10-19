package domain

import "testing"

func TestTaskAllowsOnlyDocumentedStateTransitions(t *testing.T) {
	task := Task{Status: TaskQueued}
	if err := task.Transition(TaskRunning); err != nil {
		t.Fatalf("queued to running: %v", err)
	}
	if err := task.Transition(TaskSucceeded); err != nil {
		t.Fatalf("running to succeeded: %v", err)
	}
	if err := task.Transition(TaskRunning); err == nil {
		t.Fatal("terminal task transitioned back to running")
	}
}

func TestTaskCancellationIsTerminal(t *testing.T) {
	task := Task{Status: TaskRunning}
	if err := task.Transition(TaskCancelled); err != nil {
		t.Fatalf("running to cancelled: %v", err)
	}
	if err := task.Transition(TaskSucceeded); err == nil {
		t.Fatal("cancelled task transitioned to succeeded")
	}
}
