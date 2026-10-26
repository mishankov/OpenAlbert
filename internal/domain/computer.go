package domain

import "context"

// ComputerRequest is an application-owned request to the configured machine.
// Execution IDs are stable across Activity retries; credentials never reach the model.
type ComputerRequest struct {
	Action         string `json:"action"`
	ExecutionID    string `json:"execution_id,omitempty"`
	Command        string `json:"command,omitempty"`
	Cwd            string `json:"cwd,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	WaitSeconds    int    `json:"wait_seconds"`
	StdoutOffset   int64  `json:"stdout_offset,omitempty"`
	StderrOffset   int64  `json:"stderr_offset,omitempty"`
	Path           string `json:"path,omitempty"`
	Data           []byte `json:"data,omitempty"`
}

type ComputerResult struct {
	ExecutionID     string `json:"execution_id,omitempty"`
	State           string `json:"state,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	StdoutOffset    int64  `json:"stdout_offset,omitempty"`
	StderrOffset    int64  `json:"stderr_offset,omitempty"`
	HasMore         bool   `json:"has_more,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
	Path            string `json:"path,omitempty"`
	Size            int    `json:"size,omitempty"`
	Data            []byte `json:"data,omitempty"`
	Error           string `json:"error,omitempty"`
}

type Computer interface {
	Execute(context.Context, ComputerRequest) (ComputerResult, error)
}
