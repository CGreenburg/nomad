package drainer

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"golang.org/x/time/rate"
)

var (
	// stateReadErrorDelay is the delay to apply before retrying reading state
	// when there is an error
	stateReadErrorDelay = 1 * time.Second
)

const (
	// LimitStateQueriesPerSecond is the number of state queries allowed per
	// second
	LimitStateQueriesPerSecond = 100.0

	// BatchUpdateInterval is how long we wait to batch updates
	BatchUpdateInterval = 1 * time.Second

	// NodeDeadlineCoalesceWindow is the duration in which deadlining nodes will
	// be coalesced together
	NodeDeadlineCoalesceWindow = 5 * time.Second
)

// RaftApplier contains methods for applying the raft requests required by the
// NodeDrainer.
type RaftApplier interface {
	AllocUpdateDesiredTransition(allocs map[string]*structs.DesiredTransition, evals []*structs.Evaluation) (uint64, error)
	NodeDrainComplete(nodeID string) (uint64, error)
}

// NodeTracker is the interface to notify an object that is tracking draining
// nodes of changes
type NodeTracker interface {
	// TrackedNodes returns all the nodes that are currently tracked as
	// draining.
	TrackedNodes() map[string]*structs.Node

	// Remove removes a node from the draining set.
	Remove(nodeID string)

	// Update either updates the specification of a draining node or tracks the
	// node as draining.
	Update(node *structs.Node)
}

// DrainingJobWatcherFactory returns a new DrainingJobWatcher
type DrainingJobWatcherFactory func(context.Context, *rate.Limiter, *state.StateStore, *log.Logger) DrainingJobWatcher

// DrainingNodeWatcherFactory returns a new DrainingNodeWatcher
type DrainingNodeWatcherFactory func(context.Context, *rate.Limiter, *state.StateStore, *log.Logger, NodeTracker) DrainingNodeWatcher

// DrainDeadlineNotifierFactory returns a new DrainDeadlineNotifier
type DrainDeadlineNotifierFactory func(context.Context) DrainDeadlineNotifier

// GetDrainingJobWatcher returns a draining job watcher
func GetDrainingJobWatcher(ctx context.Context, limiter *rate.Limiter, state *state.StateStore, logger *log.Logger) DrainingJobWatcher {
	return NewDrainingJobWatcher(ctx, limiter, state, logger)
}

// GetDeadlineNotifier returns a node deadline notifier with default coalescing.
func GetDeadlineNotifier(ctx context.Context) DrainDeadlineNotifier {
	return NewDeadlineHeap(ctx, NodeDeadlineCoalesceWindow)
}

// GetNodeWatcherFactory returns a DrainingNodeWatcherFactory
func GetNodeWatcherFactory() DrainingNodeWatcherFactory {
	return func(ctx context.Context, limiter *rate.Limiter, state *state.StateStore, logger *log.Logger, tracker NodeTracker) DrainingNodeWatcher {
		return NewNodeDrainWatcher(ctx, limiter, state, logger, tracker)
	}
}

// allocMigrateBatcher is used to batch allocation updates.
type allocMigrateBatcher struct {
	// updates holds pending client status updates for allocations
	updates []*structs.Allocation

	// updateFuture is used to wait for the pending batch update
	// to complete. This may be nil if no batch is pending.
	updateFuture *structs.BatchFuture

	// updateTimer is the timer that will trigger the next batch
	// update, and may be nil if there is no batch pending.
	updateTimer *time.Timer

	batchWindow time.Duration

	// synchronizes access to the updates list, the future and the timer.
	sync.Mutex
}

// NodeDrainerConfig is used to configure a new node drainer.
type NodeDrainerConfig struct {
	Logger               *log.Logger
	Raft                 RaftApplier
	JobFactory           DrainingJobWatcherFactory
	NodeFactory          DrainingNodeWatcherFactory
	DrainDeadlineFactory DrainDeadlineNotifierFactory

	// StateQueriesPerSecond configures the query limit against the state store
	// that is allowed by the node drainer.
	StateQueriesPerSecond float64

	// BatchUpdateInterval is the interval in which allocation updates are
	// batched.
	BatchUpdateInterval time.Duration
}

// NodeDrainer is used to orchestrate migrating allocations off of draining
// nodes.
type NodeDrainer struct {
	enabled bool
	logger  *log.Logger

	// nodes is the set of draining nodes
	nodes map[string]*drainingNode

	// nodeWatcher watches for nodes to transition in and out of drain state.
	nodeWatcher DrainingNodeWatcher
	nodeFactory DrainingNodeWatcherFactory

	// jobWatcher watches draining jobs and emits desired drains and notifies
	// when migrations take place.
	jobWatcher DrainingJobWatcher
	jobFactory DrainingJobWatcherFactory

	// deadlineNotifier notifies when nodes reach their drain deadline.
	deadlineNotifier        DrainDeadlineNotifier
	deadlineNotifierFactory DrainDeadlineNotifierFactory

	// state is the state that is watched for state changes.
	state *state.StateStore

	// queryLimiter is used to limit the rate of blocking queries
	queryLimiter *rate.Limiter

	// raft is a shim around the raft messages necessary for draining
	raft RaftApplier

	// batcher is used to batch alloc migrations.
	batcher allocMigrateBatcher

	// ctx and exitFn are used to cancel the watcher
	ctx    context.Context
	exitFn context.CancelFunc

	l sync.RWMutex
}

// NewNodeDrainer returns a new new node drainer. The node drainer is
// responsible for marking allocations on draining nodes with a desired
// migration transition, updating the drain strategy on nodes when they are
// complete and creating evaluations for the system to react to these changes.
func NewNodeDrainer(c *NodeDrainerConfig) *NodeDrainer {
	return &NodeDrainer{
		raft:                    c.Raft,
		logger:                  c.Logger,
		jobFactory:              c.JobFactory,
		nodeFactory:             c.NodeFactory,
		deadlineNotifierFactory: c.DrainDeadlineFactory,
		queryLimiter:            rate.NewLimiter(rate.Limit(c.StateQueriesPerSecond), 100),
		batcher: allocMigrateBatcher{
			batchWindow: c.BatchUpdateInterval,
		},
	}
}

// SetEnabled will start or stop the node draining goroutine depending on the
// enabled boolean.
func (n *NodeDrainer) SetEnabled(enabled bool, state *state.StateStore) {
	n.l.Lock()
	defer n.l.Unlock()

	// If we are starting now or have a new state, init state and start the
	// run loop
	n.enabled = enabled
	if enabled {
		n.flush(state)
		go n.run(n.ctx)
	} else if !enabled && n.exitFn != nil {
		n.exitFn()
	}
}

// flush is used to clear the state of the watcher
func (n *NodeDrainer) flush(state *state.StateStore) {
	// Cancel anything that may be running.
	if n.exitFn != nil {
		n.exitFn()
	}

	// Store the new state
	if state != nil {
		n.state = state
	}

	n.ctx, n.exitFn = context.WithCancel(context.Background())
	n.jobWatcher = n.jobFactory(n.ctx, n.queryLimiter, n.state, n.logger)
	n.nodeWatcher = n.nodeFactory(n.ctx, n.queryLimiter, n.state, n.logger, n)
	n.deadlineNotifier = n.deadlineNotifierFactory(n.ctx)
	n.nodes = make(map[string]*drainingNode, 32)
}

// run is a long lived event handler that receives changes from the relevant
// watchers and takes action based on them.
func (n *NodeDrainer) run(ctx context.Context) {
	for {
		select {
		case <-n.ctx.Done():
			return
		case nodes := <-n.deadlineNotifier.NextBatch():
			n.handleDeadlinedNodes(nodes)
		case req := <-n.jobWatcher.Drain():
			n.handleJobAllocDrain(req)
		case allocs := <-n.jobWatcher.Migrated():
			n.handleMigratedAllocs(allocs)
		}
	}
}

// handleDeadlinedNodes handles a set of nodes reaching their drain deadline.
// The handler detects the remaining allocations on the nodes and immediately
// marks them for migration.
func (n *NodeDrainer) handleDeadlinedNodes(nodes []string) {
	// Retrieve the set of allocations that will be force stopped.
	n.l.RLock()
	var forceStop []*structs.Allocation
	for _, node := range nodes {
		draining, ok := n.nodes[node]
		if !ok {
			n.logger.Printf("[DEBUG] nomad.node_drainer: skipping untracked deadlined node %q", node)
			continue
		}

		allocs, err := draining.DeadlineAllocs()
		if err != nil {
			n.logger.Printf("[ERR] nomad.node_drainer: failed to retrive allocs on deadlined node %q: %v", node, err)
			continue
		}

		forceStop = append(forceStop, allocs...)
	}
	n.l.RUnlock()
	n.batchDrainAllocs(forceStop)
}

// handleJobAllocDrain handles marking a set of allocations as having a desired
// transition to drain. The handler blocks till the changes to the allocation
// have occurred.
func (n *NodeDrainer) handleJobAllocDrain(req *DrainRequest) {
	index, err := n.batchDrainAllocs(req.Allocs)
	req.Resp.Respond(index, err)
}

// handleMigratedAllocs checks to see if any nodes can be considered done
// draining based on the set of allocations that have migrated because of an
// ongoing drain for a job.
func (n *NodeDrainer) handleMigratedAllocs(allocs []*structs.Allocation) {
	// Determine the set of nodes that were effected
	nodes := make(map[string]struct{})
	for _, alloc := range allocs {
		nodes[alloc.NodeID] = struct{}{}
	}

	// For each node, check if it is now done
	n.l.RLock()
	var done []string
	for node := range nodes {
		draining, ok := n.nodes[node]
		if !ok {
			continue
		}

		isDone, err := draining.IsDone()
		if err != nil {
			n.logger.Printf("[ERR] nomad.drain: checking if node %q is done draining: %v", node, err)
			continue
		}

		if !isDone {
			continue
		}

		done = append(done, node)
	}
	n.l.RUnlock()

	// TODO(alex) This should probably be a single Raft transaction
	for _, doneNode := range done {
		index, err := n.raft.NodeDrainComplete(doneNode)
		if err != nil {
			n.logger.Printf("[ERR] nomad.drain: failed to unset drain for node %q: %v", doneNode, err)
		} else {
			n.logger.Printf("[INFO] nomad.drain: node %q completed draining at index %d", doneNode, index)
		}
	}
}

// batchDrainAllocs is used to batch the draining of allocations. It will block
// until the batch is complete.
func (n *NodeDrainer) batchDrainAllocs(allocs []*structs.Allocation) (uint64, error) {
	// Add this to the batch
	n.batcher.Lock()
	n.batcher.updates = append(n.batcher.updates, allocs...)

	// Start a new batch if none
	future := n.batcher.updateFuture
	if future == nil {
		future = structs.NewBatchFuture()
		n.batcher.updateFuture = future
		n.batcher.updateTimer = time.AfterFunc(n.batcher.batchWindow, func() {
			// Get the pending updates
			n.batcher.Lock()
			updates := n.batcher.updates
			future := n.batcher.updateFuture
			n.batcher.updates = nil
			n.batcher.updateFuture = nil
			n.batcher.updateTimer = nil
			n.batcher.Unlock()

			// Perform the batch update
			n.drainAllocs(future, updates)
		})
	}
	n.batcher.Unlock()

	// Wait for the future
	if err := future.Wait(); err != nil {
		return 0, err
	}

	return future.Index(), nil
}

// drainAllocs is a non batch, marking of the desired transition to migrate for
// the set of allocations. It will also create the necessary evaluations for the
// affected jobs.
func (n *NodeDrainer) drainAllocs(future *structs.BatchFuture, allocs []*structs.Allocation) {
	// TODO(alex) This should shard to limit the size of the transaction.

	// Compute the effected jobs and make the transition map
	jobs := make(map[string]*structs.Allocation, 4)
	transitions := make(map[string]*structs.DesiredTransition, len(allocs))
	for _, alloc := range allocs {
		transitions[alloc.ID] = &structs.DesiredTransition{
			Migrate: helper.BoolToPtr(true),
		}
		jobs[alloc.JobID] = alloc
	}

	evals := make([]*structs.Evaluation, 0, len(jobs))
	for job, alloc := range jobs {
		evals = append(evals, &structs.Evaluation{
			ID:          uuid.Generate(),
			Namespace:   alloc.Namespace,
			Priority:    alloc.Job.Priority,
			Type:        alloc.Job.Type,
			TriggeredBy: structs.EvalTriggerNodeDrain,
			JobID:       job,
			Status:      structs.EvalStatusPending,
		})
	}

	// Commit this update via Raft
	index, err := n.raft.AllocUpdateDesiredTransition(transitions, evals)
	future.Respond(index, err)
}