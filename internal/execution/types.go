package execution

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/knotra-dev/engine/internal/contract"
)

// Role selects the work performed by a service process, without changing YAML.
type Role string

const (
	RoleAll       Role = "all"
	RoleAPI       Role = "api"
	RoleScheduler Role = "scheduler"
	RoleExecutor  Role = "executor"
)

func (r Role) Validate() error {
	switch r {
	case RoleAll, RoleAPI, RoleScheduler, RoleExecutor:
		return nil
	default:
		return errors.New("unknown execution process role")
	}
}

// RunResult contains the terminal state and validated outputs of a run.
type RunResult struct {
	Status  string          `json:"status"`
	Outputs contract.Values `json:"outputs,omitempty"`
	Failure *Failure        `json:"failure,omitempty"`
}

type BudgetScope struct {
	ID     string          `json:"id"`
	Limits contract.Limits `json:"limits"`
}

type ExecuteRequest struct {
	RunID         string          `json:"runId"`
	InstanceID    string          `json:"instanceId"`
	NodeID        string          `json:"nodeId"`
	Pipeline      string          `json:"pipeline"`
	Attempt       int             `json:"attempt"`
	Node          contract.Node   `json:"node"`
	Inputs        contract.Values `json:"inputs"`
	ToolArguments json.RawMessage `json:"toolArguments,omitempty"`
	// Plan is loaded by the executor from the admitted run snapshot.
	// Delivery messages omit it to avoid copying the source package per attempt.
	Plan   contract.Plan `json:"plan,omitzero"`
	Scopes []BudgetScope `json:"scopes"`
	// Permissions is the intersection of all caller boundaries. Nil means the
	// root document, which is still constrained by the trusted profile.
	Permissions *contract.Permissions `json:"permissions,omitempty"`
	Deadline    time.Time             `json:"deadline"`
	Ownership   *Ownership            `json:"ownership,omitempty"`
}

type ExecuteResult struct {
	Outputs contract.Values `json:"outputs,omitempty"`
	Failure *Failure        `json:"failure,omitempty"`
}

type Projection struct {
	ParentInstanceID string          `json:"parentInstanceId,omitempty"`
	IterationIndex   *int            `json:"iterationIndex,omitempty"`
	GraphPath        string          `json:"graphPath,omitempty"`
	NodeType         string          `json:"nodeType,omitempty"`
	Inputs           contract.Values `json:"inputs,omitempty"`
	DataTruncated    bool            `json:"dataTruncated,omitempty"`
	StartedAt        *time.Time      `json:"startedAt,omitempty"`
	FinishedAt       *time.Time      `json:"finishedAt,omitempty"`

	RunID      string          `json:"runId"`
	Sequence   int64           `json:"sequence"`
	Time       time.Time       `json:"time"`
	Kind       string          `json:"kind"`
	InstanceID string          `json:"instanceId,omitempty"`
	NodeID     string          `json:"nodeId,omitempty"`
	Pipeline   string          `json:"pipeline,omitempty"`
	Status     string          `json:"status"`
	Attempt    int             `json:"attempt,omitempty"`
	Outputs    contract.Values `json:"outputs,omitempty"`
	Failure    *Failure        `json:"failure,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	RequestID  string          `json:"requestId,omitempty"`
	ResponseID string          `json:"responseId,omitempty"`
}

type Request struct {
	RunID      string                   `json:"runId"`
	ID         string                   `json:"id"`
	InstanceID string                   `json:"instanceId"`
	Kind       string                   `json:"kind"`
	Status     string                   `json:"status"`
	Prompt     string                   `json:"prompt,omitempty"`
	Inputs     contract.Values          `json:"inputs,omitempty"`
	Outputs    map[string]contract.Port `json:"outputs,omitempty"`
	Failure    *Failure                 `json:"failure,omitempty"`
	Deadline   time.Time                `json:"deadline"`
	ResponseID string                   `json:"responseId,omitempty"`
	Response   contract.Values          `json:"response,omitempty"`
	Evidence   string                   `json:"evidence,omitempty"`
}

type HumanSignal struct {
	RequestID  string          `json:"requestId"`
	ResponseID string          `json:"responseId"`
	Values     contract.Values `json:"values"`
	AcceptedAt time.Time       `json:"acceptedAt,omitempty"`
}

type ResolutionSignal struct {
	InstanceID  string          `json:"instanceId"`
	OperationID string          `json:"operationId"`
	ResponseID  string          `json:"responseId"`
	Decision    string          `json:"decision"` // completed, not_executed, or failed
	Outputs     contract.Values `json:"outputs,omitempty"`
	Evidence    string          `json:"evidence"`
	AcceptedAt  time.Time       `json:"acceptedAt,omitempty"`
}

type CancelSignal struct {
	Reason string `json:"reason,omitempty"`
}
