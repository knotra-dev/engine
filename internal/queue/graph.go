package queue

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/scheduler"
	"github.com/knotra-dev/engine/internal/store"
	"github.com/knotra-dev/engine/internal/store/db"
)

func (r *Runtime) advanceGraphTx(ctx context.Context, tx pgx.Tx, plan contract.Plan, runID, graphID, focusInstanceID string, bound int) (changes scheduler.Changes, err error) {
	graph, err := store.ReadExecutionGraphMetadata(ctx, tx, runID, graphID)
	if err != nil {
		return changes, err
	}
	run, err := store.LockExecutionRun(ctx, tx, runID)
	if err != nil {
		return changes, err
	}
	definition, err := execution.GraphDefinition(plan, graph.Pipeline, graph.GraphPath)
	if err != nil {
		return changes, err
	}
	nodes, err := store.ReadSchedulableNodes(ctx, tx, run, graph, focusInstanceID, bound)
	if err != nil {
		return changes, err
	}
	instanceIDs, nodeIDs := make([]string, 0, len(nodes)), make([]string, 0, len(nodes))
	var dependencies []string
	for _, node := range nodes {
		instanceIDs = append(instanceIDs, node.ID)
		nodeIDs = append(nodeIDs, node.NodeID)
		if node.State == "pending" {
			dependencies = append(dependencies, definition.Nodes[node.NodeID].Dependencies...)
		}
	}
	if graph.State == "pending" {
		graph.Inputs, err = store.ReadExecutionGraphInputValues(ctx, tx, runID, graph.ID, nil)
	} else {
		// Admission may finish materialization and admit new nodes in this step.
		// Load only statically referenced graph inputs, including conditions.
		needed := map[string]bool{}
		if graph.MaterializationCursor == len(definition.Nodes) {
			for _, node := range nodes {
				if node.State == "pending" {
					needed[node.NodeID] = true
				}
			}
		} else if graph.MaterializationCursor+bound > len(definition.Nodes) {
			for _, node := range nodes {
				if node.State == "pending" {
					needed[node.NodeID] = true
				}
			}
			ids := make([]string, 0, len(definition.Nodes))
			for id := range definition.Nodes {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			for _, id := range ids[graph.MaterializationCursor:] {
				needed[id] = true
			}
		}
		var bindings []contract.Binding
		for id := range needed {
			definitionNode := definition.Nodes[id]
			for _, port := range definitionNode.Inputs {
				if port.Bind != nil {
					bindings = append(bindings, *port.Bind)
				}
			}
			if definitionNode.When != "" {
				bindings = append(bindings, contract.Binding{Expr: definitionNode.When})
			}
		}
		var names []string
		names, err = inputReferenceNames(bindings, "inputs")
		if err == nil {
			graph.Inputs, err = store.ReadExecutionGraphInputValues(ctx, tx, runID, graph.ID, names)
		}
	}
	if err != nil {
		return changes, err
	}
	slices.Sort(dependencies)
	dependencyValues, err := store.ReadExecutionDependencyValues(ctx, tx, runID, graph.ID, slices.Compact(dependencies))
	if err != nil {
		return changes, err
	}
	attempts, err := store.ReadExecutionAttempts(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	scopes, err := store.ReadExecutionScopes(ctx, tx, graph)
	if err != nil {
		return changes, err
	}
	timers, err := store.ReadExecutionRetryTimers(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	requests, err := store.ReadExecutionRequests(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	controls, err := store.ReadExecutionControls(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	controlByID := make(map[string]execution.ControlRecord, len(controls))
	for _, control := range controls {
		controlByID[control.InstanceID] = control
	}
	for i := range nodes {
		node := &nodes[i]
		if !node.InputsOmitted {
			continue
		}
		frozen := definition.Nodes[node.NodeID]
		control, found := controlByID[node.ID]
		if !found || frozen.Foreach == nil {
			return changes, errors.New("foreach snapshot has no frozen control")
		}
		if !run.Paused && run.StopCause == nil && !run.Cancelled && control.NextPosition < control.ElementCount && control.ActiveChildren < control.Concurrency {
			bindings := make([]contract.Binding, 0, len(frozen.Foreach.With))
			for _, binding := range frozen.Foreach.With {
				bindings = append(bindings, binding)
			}
			names, err := inputReferenceNames(bindings, "args")
			if err != nil {
				return changes, err
			}
			node.Inputs, err = store.ReadExecutionNodeInputValues(ctx, tx, runID, node.ID, names)
			if err != nil {
				return changes, err
			}
		}
		frozen.Execution = execution.ExecutionPolicy(plan.Pipelines[graph.Pipeline].Spec.Defaults.Execution, frozen.Execution)
		node.Request = &execution.ExecuteRequest{RunID: runID, InstanceID: node.ID, NodeID: node.NodeID, Pipeline: graph.Pipeline,
			Attempt: 1, Node: frozen, Inputs: node.Inputs, Scopes: graph.Scopes, Permissions: graph.Permissions, Deadline: node.Deadline}
	}
	children, err := store.ReadChildExecutionGraphs(ctx, tx, runID, graph.ID, instanceIDs)
	if err != nil {
		return changes, err
	}
	unresolved, err := db.New(tx).HasUnresolvedOutsideSelection(ctx, db.HasUnresolvedOutsideSelectionParams{RunID: runID, InstanceIds: instanceIDs})
	if err != nil {
		return changes, err
	}
	active, err := db.New(tx).HasActiveNodesOutsideSelection(ctx, db.HasActiveNodesOutsideSelectionParams{RunID: runID, GraphID: graph.ID, InstanceIds: instanceIDs})
	if err != nil {
		return changes, err
	}
	snapshot := scheduler.Snapshot{Graph: graph, Nodes: append(dependencyValues, nodes...), Attempts: attempts, Scopes: scopes,
		Timers: timers, Requests: requests, Controls: controls, Children: children, UnresolvedElsewhere: unresolved,
		FocusInstanceID: focusInstanceID, SelectedNodeIDs: nodeIDs, ActiveNodesElsewhere: active}
	// At most two pure preparation passes: a selected foreach's collection,
	// then the graph's export references. No proposed mutation is saved yet.
	for preparation := 0; ; preparation++ {
		if preparation > 2 {
			return changes, errors.New("scheduler requested unprepared complete values")
		}
		// Record loading, scope locks and complete exports may cross a deadline.
		snapshot.Run, err = store.LockExecutionRun(ctx, tx, runID)
		if err != nil {
			return changes, err
		}
		changes, err = scheduler.Step(plan, snapshot, bound)
		schedulerCount(ctx, "steps")
		if err != nil {
			return changes, err
		}
		if len(changes.ForeachCollections) == 0 && !changes.GraphExports {
			run = snapshot.Run
			break
		}
		if len(changes.ForeachCollections) > 0 {
			nodeByID := make(map[string]execution.NodeRecord, len(nodes))
			controlByID := make(map[string]execution.ControlRecord, len(controls))
			for _, node := range nodes {
				nodeByID[node.ID] = node
			}
			for _, control := range controls {
				controlByID[control.InstanceID] = control
			}
			if snapshot.ForeachOutputs == nil {
				snapshot.ForeachOutputs = map[string]contract.Values{}
			}
			for _, id := range changes.ForeachCollections {
				node := nodeByID[id]
				if node.Request == nil || node.Request.Node.Foreach == nil {
					return changes, errors.New("foreach node has no frozen collection definition")
				}
				outputs, err := store.ReadForeachCollectedOutputs(ctx, tx, controlByID[id], node.Request.Node.Foreach.Body)
				if err != nil {
					return changes, err
				}
				snapshot.ForeachOutputs[id] = outputs
			}
		}
		if changes.GraphExports {
			dependencies, err := contract.PortDependencies(definition.Outputs)
			if err != nil {
				return changes, err
			}
			exports, err := store.ReadExecutionDependencyValues(ctx, tx, runID, graph.ID, dependencies)
			if err != nil {
				return changes, err
			}
			bindings := make([]contract.Binding, 0, len(definition.Outputs))
			for _, port := range definition.Outputs {
				if port.Bind != nil {
					bindings = append(bindings, *port.Bind)
				}
			}
			names, err := inputReferenceNames(bindings, "inputs")
			if err != nil {
				return changes, err
			}
			inputValues, err := store.ReadExecutionGraphInputValues(ctx, tx, runID, graph.ID, names)
			if err != nil {
				return changes, err
			}
			if snapshot.Graph.Inputs == nil {
				snapshot.Graph.Inputs = contract.Values{}
			}
			for name, value := range inputValues {
				snapshot.Graph.Inputs[name] = value
			}
			snapshot.Nodes = append(exports, snapshot.Nodes...)
			snapshot.GraphExportsLoaded = true
		}
	}
	if changes.ReserveInstances > 0 {
		for _, scope := range scopes {
			tag, err := db.New(tx).ReserveNodeInstances(ctx, db.ReserveNodeInstancesParams{RunID: runID, ID: scope.ID, MaterializedInstances: changes.ReserveInstances})
			if err != nil {
				return changes, err
			}
			if tag.RowsAffected() != 1 {
				return changes, store.ErrConflict
			}
		}
	}
	projections := make([]execution.Projection, 0, len(changes.Nodes)+2)
	if changes.StartRun {
		projections = append(projections, execution.Projection{RunID: runID, Kind: "run", Status: "running", Time: run.Now})
	}
	if changes.Graph != nil {
		if err := store.SaveExecutionGraph(ctx, tx, *changes.Graph); err != nil {
			return changes, err
		}
	}
	for i, node := range changes.Nodes {
		if node.InputsOmitted {
			original, err := store.ReadExecutionNode(ctx, tx, runID, node.ID)
			if err != nil {
				return changes, err
			}
			if original.Request == nil || original.Revision != node.Revision-1 {
				return changes, store.ErrConflict
			}
			node.Inputs, node.Request, node.InputsOmitted = original.Inputs, original.Request, false
			changes.Nodes[i] = node
		}
		if err := store.SaveExecutionNode(ctx, tx, node, definition.Nodes[node.NodeID]); err != nil {
			return changes, err
		}
		if !node.Deadline.IsZero() {
			if _, err := db.New(tx).InsertNodeDeadline(ctx, db.InsertNodeDeadlineParams{RunID: runID, ID: "node/" + node.ID, InstanceID: new(node.ID), DueAt: node.Deadline}); err != nil {
				return changes, err
			}
		}
		projection := execution.Projection{RunID: runID, Kind: "node", InstanceID: node.ID, NodeID: node.NodeID, Pipeline: graph.Pipeline,
			ParentInstanceID: graph.ParentInstanceID, IterationIndex: graph.IterationIndex, GraphPath: graph.GraphPath + "/nodes/" + node.NodeID,
			NodeType: definition.Nodes[node.NodeID].Type, Status: node.State, Attempt: node.AttemptNumber,
			Inputs: node.Inputs, Outputs: node.Outputs, Failure: node.Failure, StartedAt: node.StartedAt, FinishedAt: node.FinishedAt, Reason: node.Reason, Time: run.Now}
		projections = append(projections, projection)
	}
	for _, scope := range changes.NewScopes {
		if err := store.InsertExecutionScope(ctx, tx, scope); err != nil {
			return changes, err
		}
	}
	for _, child := range changes.Children {
		if err := store.InsertExecutionGraph(ctx, tx, child); err != nil {
			return changes, err
		}
	}
	for _, control := range changes.Controls {
		if err := store.SaveExecutionControl(ctx, tx, control); err != nil {
			return changes, err
		}
	}
	for _, request := range changes.Requests {
		if err := r.Store.SaveExecutionRequest(ctx, tx, request); err != nil {
			return changes, err
		}
	}
	for _, timer := range changes.Timers {
		if _, err := db.New(tx).InsertExecutionTimer(ctx, db.InsertExecutionTimerParams{
			RunID:      timer.RunID,
			ID:         timer.ID,
			Generation: timer.Generation,
			InstanceID: new(timer.InstanceID),
			Kind:       timer.Kind,
			DueAt:      timer.DueAt,
		}); err != nil {
			return changes, err
		}
	}
	if changes.Pause {
		if _, err := db.New(tx).PauseRunAdmission(ctx, runID); err != nil {
			return changes, err
		}
		if !run.Paused {
			projections = append(projections, execution.Projection{RunID: runID, Kind: "run", Status: "waiting_resolution", Failure: changes.PauseCause, Time: run.Now})
		}
	}
	if changes.Resume {
		if _, err := db.New(tx).ResumeRunAdmission(ctx, runID); err != nil {
			return changes, err
		}
		projections = append(projections, execution.Projection{RunID: runID, Kind: "run", Status: "running", Time: run.Now})
	}
	if err := r.Store.ProjectExecutionBatch(ctx, tx, projections); err != nil {
		return changes, err
	}
	if changes.StopCause != nil && run.StopCause == nil {
		b, err := json.Marshal(changes.StopCause)
		if err != nil {
			return changes, err
		}
		if _, err := db.New(tx).SetRunStopCause(ctx, db.SetRunStopCauseParams{ID: runID, StopCause: b}); err != nil {
			return changes, err
		}
	}
	for _, attempt := range changes.Attempts {
		if err := store.InsertExecutionAttempt(ctx, tx, attempt); err != nil {
			return changes, err
		}
		if err := r.Client.InsertExecute(ctx, tx, ExecuteArgs{Attempt: attempt.AttemptID, DispatchGeneration: attempt.DispatchGeneration, RoutingVersion: RoutingVersion}); err != nil {
			return changes, err
		}
		if _, err := db.New(tx).ClearAttemptDispatch(ctx, db.ClearAttemptDispatchParams{RunID: attempt.RunID, InstanceID: attempt.InstanceID, Number: attempt.Number}); err != nil {
			return changes, err
		}
	}
	// A capacity miss keeps the unstarted business attempt and changes only its
	// delivery generation. Completed River jobs cannot suppress this redispatch.
	for _, attempt := range attempts {
		if attempt.State != "ready" || !attempt.DispatchPending || (run.Paused && !changes.Resume) || changes.Pause || changes.StopCause != nil {
			continue
		}
		if changes.Transitions >= bound {
			changes.More = true
			break
		}
		changes.Transitions++
		generation := attempt.DispatchGeneration + 1
		if _, err := db.New(tx).SetAttemptDispatchGeneration(ctx, db.SetAttemptDispatchGenerationParams{
			RunID:              attempt.RunID,
			InstanceID:         attempt.InstanceID,
			Number:             attempt.Number,
			DispatchGeneration: generation,
		}); err != nil {
			return changes, err
		}
		if err := r.Client.InsertExecute(ctx, tx, ExecuteArgs{Attempt: attempt.AttemptID, DispatchGeneration: generation, RoutingVersion: RoutingVersion}); err != nil {
			return changes, err
		}
	}
	return changes, nil
}

// Static references let complete input values remain durable without returning
// unrelated arrays on every control transition. Dynamic names are rejected by
// the contract compiler's shared checked-expression analysis.
func inputReferenceNames(bindings []contract.Binding, namespace string) ([]string, error) {
	names := map[string]bool{}
	for _, binding := range bindings {
		refs, err := contract.BindingReferences(binding)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if strings.HasPrefix(ref, namespace+".") {
				names[strings.Split(ref, ".")[1]] = true
			}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	slices.Sort(result)
	return result, nil
}
