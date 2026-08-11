package blockstm

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	kernel "github.com/crypto-org-chain/go-block-stm"
	"github.com/crypto-org-chain/go-block-stm/internal/control"
	engineapi "github.com/crypto-org-chain/go-block-stm/internal/engine"
	"github.com/crypto-org-chain/go-block-stm/internal/model"
)

// dependencyPreparation is built inside the timed ExecuteBlock call. All
// static modes acquire the same conservative program access information;
// only their representation and scheduling use differ.
type dependencyPreparation struct {
	base           control.DependencyCounters
	controller     *dependencyController
	estimates      []kernel.MultiLocations
	consumerActive bool
}

func prepareDependency(
	ctx context.Context,
	block model.Block,
	plan engineapi.DependencyPlan,
	storeIndex int,
	capture bool,
) (dependencyPreparation, error) {
	artifact, err := acquireDependencyInformation(ctx, block, plan.Source, capture)
	if err != nil {
		return dependencyPreparation{}, err
	}
	preparation := dependencyPreparation{base: artifact.counters(plan)}
	if plan.Source == control.DependencySourceRuntimeObserved {
		preparation.base.AcquisitionDisposition = control.DependencyDispositionRuntimeKernel
		return preparation, nil
	}

	var representation dependencyRepresentationArtifact
	if plan.Representation != control.DependencyRepresentationVersionOnly {
		representationStarted := measuredStart(capture)
		representation, err = buildDependencyRepresentation(ctx, artifact.staticAccesses, plan)
		if err != nil {
			return dependencyPreparation{}, err
		}
		preparation.base.RepresentationMeasured = capture
		preparation.base.RepresentationNS = measuredElapsed(representationStarted)
		preparation.base.RepresentationBuildUnits = representation.buildUnits
		preparation.base.RepresentationLogicalBytes = representation.logicalBytes
		preparation.base.RepresentationEntries = representation.entries
		preparation.base.RepresentationMaxFanIn = representation.maxFanIn
		preparation.base.DependencyEdges = representation.edges
		preparation.base.SummaryEntries = representation.summaryEntries
	}

	preparation.consumerActive = plan.WaitPolicy != control.DependencyWaitNone ||
		plan.EstimateInjection != control.DependencyEstimatesDisabled
	switch {
	case plan.WaitPolicy != control.DependencyWaitNone:
		preparation.base.AcquisitionDisposition = control.DependencyDispositionRepresentation
	case plan.Representation != control.DependencyRepresentationVersionOnly:
		preparation.base.AcquisitionDisposition = control.DependencyDispositionRepresentationDiscarded
	case preparation.consumerActive:
		preparation.base.AcquisitionDisposition = control.DependencyDispositionAcquisitionConsumer
	default:
		preparation.base.AcquisitionDisposition = control.DependencyDispositionDiscarded
	}

	if plan.WaitPolicy != control.DependencyWaitNone {
		var gate dependencyGate
		if plan.WaitPolicy == control.DependencyWaitContiguousFrontier {
			gate = newSummaryDependencyGate(representation.barriers)
		} else {
			gate = newExplicitDependencyGate(representation.predecessors)
		}
		preparation.controller = &dependencyController{gate: gate, capture: capture}
		preparation.base.ResolutionMeasured = capture
	}
	if plan.EstimateInjection == control.DependencyEstimatesWrite {
		estimateStarted := measuredStart(capture)
		preparation.estimates, preparation.base.EstimatedWriteLocations, preparation.base.EstimatedWriteKeyBytes, err =
			buildWriteEstimates(ctx, artifact.staticAccesses, storeIndex)
		if err != nil {
			return dependencyPreparation{}, err
		}
		preparation.base.EstimateBuildNS = measuredElapsed(estimateStarted)
	}
	return preparation, nil
}

func measuredStart(capture bool) time.Time {
	if !capture {
		return time.Time{}
	}
	return time.Now()
}

func measuredElapsed(start time.Time) uint64 {
	if start.IsZero() {
		return 0
	}
	return uint64(time.Since(start))
}

type staticAcquisition struct {
	complete  bool
	units     uint64
	bytes     uint64
	readKeys  uint64
	writeKeys uint64
}

type dependencyInformationArtifact struct {
	source         control.DependencySource
	availableAt    string
	version        string
	complete       bool
	exact          bool
	measured       bool
	elapsedNS      uint64
	units          uint64
	bytes          uint64
	readKeys       uint64
	writeKeys      uint64
	staticAccesses []staticAccessSet
}

func acquireDependencyInformation(
	ctx context.Context,
	block model.Block,
	source control.DependencySource,
	capture bool,
) (dependencyInformationArtifact, error) {
	if source == control.DependencySourceRuntimeObserved {
		return dependencyInformationArtifact{
			source:      source,
			availableAt: control.DependencyAvailableDuringExecution,
			version:     control.DependencySourceVersionRuntimeMVCC,
			complete:    true,
			exact:       true,
		}, nil
	}

	started := measuredStart(capture)
	accesses, acquisition, err := analyzeStaticPrograms(ctx, block)
	if err != nil {
		return dependencyInformationArtifact{}, err
	}
	return dependencyInformationArtifact{
		source:         source,
		availableAt:    control.DependencyAvailableBeforeExecution,
		version:        control.DependencySourceVersionStaticScan,
		complete:       acquisition.complete,
		exact:          false,
		measured:       capture,
		elapsedNS:      measuredElapsed(started),
		units:          acquisition.units,
		bytes:          acquisition.bytes,
		readKeys:       acquisition.readKeys,
		writeKeys:      acquisition.writeKeys,
		staticAccesses: accesses,
	}, nil
}

func (a dependencyInformationArtifact) counters(plan engineapi.DependencyPlan) control.DependencyCounters {
	return control.DependencyCounters{
		Mode:                  plan.Mode,
		Source:                a.source,
		Representation:        plan.Representation,
		RepresentationBuilder: plan.RepresentationBuilder,
		WaitPolicy:            plan.WaitPolicy,
		EstimateInjection:     plan.EstimateInjection,
		SourceAvailableAt:     a.availableAt,
		SourceVersion:         a.version,
		InformationComplete:   a.complete,
		InformationExact:      a.exact,
		AcquisitionMeasured:   a.measured,
		AcquisitionNS:         a.elapsedNS,
		AcquisitionUnits:      a.units,
		AcquisitionBytes:      a.bytes,
		StaticReadKeys:        a.readKeys,
		StaticWriteKeys:       a.writeKeys,
	}
}

type staticAccessSet struct {
	reads  []string
	writes []string
}

func analyzeStaticPrograms(ctx context.Context, block model.Block) ([]staticAccessSet, staticAcquisition, error) {
	accesses := make([]staticAccessSet, len(block.Transactions))
	acquisition := staticAcquisition{complete: true}
	for transactionIndex, transaction := range block.Transactions {
		if transactionIndex&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, staticAcquisition{}, err
			}
		}
		reads := make(map[string]struct{})
		writes := make(map[string]struct{})
		for instructionIndex, instruction := range transaction.Program.Instructions {
			if instructionIndex&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, staticAcquisition{}, err
				}
			}
			acquisition.units++
			switch instruction.Op {
			case model.OpRead:
				reads[string(instruction.Key)] = struct{}{}
				acquisition.bytes += uint64(len(instruction.Key))
			case model.OpWrite, model.OpDelete:
				writes[string(instruction.Key)] = struct{}{}
				acquisition.bytes += uint64(len(instruction.Key))
			case model.OpCompute, model.OpFailIf, model.OpJumpIf, model.OpReturn:
				// These flat-runtime instructions perform no hidden state access.
			default:
				// A future/unknown opcode may access state dynamically. Static
				// guidance remains a hint; Block-STM validation repairs misses.
				acquisition.complete = false
			}
		}
		accesses[transactionIndex] = staticAccessSet{
			reads:  sortedSet(reads),
			writes: sortedSet(writes),
		}
		acquisition.readKeys += uint64(len(reads))
		acquisition.writeKeys += uint64(len(writes))
	}
	return accesses, acquisition, nil
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

type dependencyRepresentationArtifact struct {
	predecessors   [][]int
	barriers       []int
	edges          uint64
	summaryEntries uint64
	entries        uint64
	maxFanIn       uint64
	buildUnits     uint64
	logicalBytes   uint64
}

func buildDependencyRepresentation(
	ctx context.Context,
	accesses []staticAccessSet,
	plan engineapi.DependencyPlan,
) (dependencyRepresentationArtifact, error) {
	switch plan.Representation {
	case control.DependencyRepresentationRAWLastWriter:
		predecessors, edges, err := buildDeclaredRAWDAG(ctx, accesses)
		if err != nil {
			return dependencyRepresentationArtifact{}, err
		}
		return dependencyRepresentationArtifact{
			predecessors: predecessors,
			edges:        edges,
			entries:      edges,
			maxFanIn:     maximumFanIn(predecessors),
			buildUnits:   staticAccessUnits(accesses),
			logicalBytes: uint64(len(predecessors))*8 + edges*8,
		}, nil
	case control.DependencyRepresentationMaxRAWPredecessor:
		barriers, entries, err := buildDependencySummary(ctx, accesses)
		if err != nil {
			return dependencyRepresentationArtifact{}, err
		}
		maxFanIn := uint64(0)
		if entries > 0 {
			maxFanIn = 1
		}
		return dependencyRepresentationArtifact{
			barriers:       barriers,
			summaryEntries: entries,
			entries:        entries,
			maxFanIn:       maxFanIn,
			buildUnits:     staticAccessUnits(accesses),
			logicalBytes:   uint64(len(barriers)) * 8,
		}, nil
	case control.DependencyRepresentationFullConflictGraph:
		var predecessors [][]int
		var edges uint64
		var units uint64
		var err error
		if plan.RepresentationBuilder == control.DependencyRepresentationBuilderIndexedByKey {
			predecessors, edges, units, err = buildFullConflictGraphIndexed(ctx, accesses)
		} else {
			predecessors, edges, err = buildFullConflictGraph(ctx, accesses)
			units = uint64(len(accesses)*(len(accesses)-1)) / 2
		}
		if err != nil {
			return dependencyRepresentationArtifact{}, err
		}
		return dependencyRepresentationArtifact{
			predecessors: predecessors,
			edges:        edges,
			entries:      edges,
			maxFanIn:     maximumFanIn(predecessors),
			buildUnits:   units,
			logicalBytes: uint64(len(predecessors))*8 + edges*8,
		}, nil
	default:
		return dependencyRepresentationArtifact{}, nil
	}
}

func staticAccessUnits(accesses []staticAccessSet) uint64 {
	var units uint64
	for _, access := range accesses {
		units += uint64(len(access.reads) + len(access.writes))
	}
	return units
}

func maximumFanIn(predecessors [][]int) uint64 {
	var maximum uint64
	for _, values := range predecessors {
		if uint64(len(values)) > maximum {
			maximum = uint64(len(values))
		}
	}
	return maximum
}

// buildDeclaredRAWDAG constructs the minimal direct read-after-write guidance
// available from statically named accesses. Branches are conservatively
// over-approximated because every syntactic access is included.
func buildDeclaredRAWDAG(ctx context.Context, accesses []staticAccessSet) ([][]int, uint64, error) {
	predecessors := make([][]int, len(accesses))
	lastWriter := make(map[string]int)
	var edges uint64
	for transaction, access := range accesses {
		if transaction&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		seen := make(map[int]struct{})
		for _, key := range access.reads {
			if predecessor, exists := lastWriter[key]; exists {
				seen[predecessor] = struct{}{}
			}
		}
		if len(seen) > 0 {
			predecessors[transaction] = sortedIndices(seen)
		}
		edges += uint64(len(predecessors[transaction]))
		for _, key := range access.writes {
			lastWriter[key] = transaction
		}
	}
	return predecessors, edges, nil
}

// buildDependencySummary stores only the greatest direct RAW predecessor per
// transaction. Its use waits for the contiguous execution-complete frontier,
// trading a compact representation for conservative serialization.
func buildDependencySummary(ctx context.Context, accesses []staticAccessSet) ([]int, uint64, error) {
	barriers := make([]int, len(accesses))
	for index := range barriers {
		barriers[index] = -1
	}
	lastWriter := make(map[string]int)
	var entries uint64
	for transaction, access := range accesses {
		if transaction&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		for _, key := range access.reads {
			if predecessor, exists := lastWriter[key]; exists && predecessor > barriers[transaction] {
				barriers[transaction] = predecessor
			}
		}
		if barriers[transaction] >= 0 {
			entries++
		}
		for _, key := range access.writes {
			lastWriter[key] = transaction
		}
	}
	return barriers, entries, nil
}

// buildFullConflictGraph materializes every preset-order RAW, WAR, and WAW
// pair. Edges always point from a lower to a higher transaction index, so the
// guidance graph is acyclic and cannot change canonical order.
func buildFullConflictGraph(ctx context.Context, accesses []staticAccessSet) ([][]int, uint64, error) {
	predecessors := make([][]int, len(accesses))
	var edges uint64
	for later := 0; later < len(accesses); later++ {
		for earlier := 0; earlier < later; earlier++ {
			if earlier&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, 0, err
				}
			}
			if conflict(accesses[earlier], accesses[later]) {
				predecessors[later] = append(predecessors[later], earlier)
				edges++
			}
		}
	}
	return predecessors, edges, nil
}

// buildFullConflictGraphIndexed constructs the same preset-order RAW/WAR/WAW
// graph as the quadratic reference, but visits only prior readers and writers
// of keys accessed by the current transaction.
func buildFullConflictGraphIndexed(ctx context.Context, accesses []staticAccessSet) ([][]int, uint64, uint64, error) {
	predecessors := make([][]int, len(accesses))
	readers := make(map[string][]int)
	writers := make(map[string][]int)
	var edges uint64
	var units uint64
	for transaction, access := range accesses {
		if transaction&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
		}
		seen := make(map[int]struct{})
		for _, key := range access.reads {
			priorWriters := writers[key]
			units += 1 + uint64(len(priorWriters))
			for _, predecessor := range priorWriters {
				seen[predecessor] = struct{}{}
			}
		}
		for _, key := range access.writes {
			priorReaders := readers[key]
			priorWriters := writers[key]
			units += 1 + uint64(len(priorReaders)+len(priorWriters))
			for _, predecessor := range priorReaders {
				seen[predecessor] = struct{}{}
			}
			for _, predecessor := range priorWriters {
				seen[predecessor] = struct{}{}
			}
		}
		if len(seen) > 0 {
			predecessors[transaction] = sortedIndices(seen)
		}
		edges += uint64(len(predecessors[transaction]))
		for _, key := range access.reads {
			readers[key] = append(readers[key], transaction)
		}
		for _, key := range access.writes {
			writers[key] = append(writers[key], transaction)
		}
	}
	return predecessors, edges, units, nil
}

func conflict(earlier, later staticAccessSet) bool {
	return intersects(earlier.writes, later.reads) ||
		intersects(earlier.writes, later.writes) ||
		intersects(earlier.reads, later.writes)
}

func intersects(left, right []string) bool {
	i, j := 0, 0
	for i < len(left) && j < len(right) {
		switch {
		case left[i] < right[j]:
			i++
		case left[i] > right[j]:
			j++
		default:
			return true
		}
	}
	return false
}

func sortedIndices(values map[int]struct{}) []int {
	result := make([]int, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Ints(result)
	return result
}

func buildWriteEstimates(ctx context.Context, accesses []staticAccessSet, storeIndex int) ([]kernel.MultiLocations, uint64, uint64, error) {
	estimates := make([]kernel.MultiLocations, len(accesses))
	var locations uint64
	var keyBytes uint64
	for transaction, access := range accesses {
		if transaction&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
		}
		if len(access.writes) == 0 {
			continue
		}
		keys := make(kernel.Locations, len(access.writes))
		for index, key := range access.writes {
			keys[index] = kernel.Key([]byte(key))
			keyBytes += uint64(len(key))
		}
		estimates[transaction] = kernel.MultiLocations{storeIndex: keys}
		locations += uint64(len(keys))
	}
	return estimates, locations, keyBytes, nil
}

type dependencyGate interface {
	Wait(context.Context, int, bool) (traversal uint64, events uint64, elapsed uint64, err error)
	Complete(int)
}

type dependencyController struct {
	gate    dependencyGate
	capture bool

	lookups    atomic.Uint64
	resolution atomic.Uint64
	traversal  atomic.Uint64
	waited     atomic.Uint64
	waitEvents atomic.Uint64
	waitNS     atomic.Uint64
}

func (c *dependencyController) Wait(ctx context.Context, transaction int) error {
	if c == nil || c.gate == nil {
		return nil
	}
	c.lookups.Add(1)
	started := measuredStart(c.capture)
	traversal, events, elapsed, err := c.gate.Wait(ctx, transaction, c.capture)
	c.resolution.Add(measuredElapsed(started))
	c.traversal.Add(traversal)
	if events > 0 {
		c.waited.Add(1)
		c.waitEvents.Add(events)
		c.waitNS.Add(elapsed)
	}
	return err
}

func (c *dependencyController) Complete(transaction int) {
	if c != nil && c.gate != nil {
		c.gate.Complete(transaction)
	}
}

func (p dependencyPreparation) Counters(reexecutions, reexecutionUnits uint64) control.DependencyCounters {
	counters := p.base
	if p.controller != nil {
		counters.PlanLookups = p.controller.lookups.Load()
		counters.ResolutionNS = p.controller.resolution.Load()
		counters.TraversalSteps = p.controller.traversal.Load()
		counters.WaitedExecutionAttempts = p.controller.waited.Load()
		counters.WaitEvents = p.controller.waitEvents.Load()
		counters.WaitNS = p.controller.waitNS.Load()
	}
	if p.consumerActive {
		counters.PostGuidanceReexecutions = reexecutions
		counters.PostGuidanceReexecutionUnits = reexecutionUnits
	}
	return counters
}

type explicitDependencyGate struct {
	predecessors [][]int
	done         []chan struct{}
	once         []sync.Once
}

func newExplicitDependencyGate(predecessors [][]int) *explicitDependencyGate {
	done := make([]chan struct{}, len(predecessors))
	for index := range done {
		done[index] = make(chan struct{})
	}
	return &explicitDependencyGate{
		predecessors: predecessors,
		done:         done,
		once:         make([]sync.Once, len(predecessors)),
	}
}

func (g *explicitDependencyGate) Wait(ctx context.Context, transaction int, capture bool) (uint64, uint64, uint64, error) {
	var traversal uint64
	var events uint64
	var started time.Time
	predecessors := g.predecessors[transaction]
	for _, predecessor := range predecessors {
		traversal++
		select {
		case <-g.done[predecessor]:
			continue
		default:
			events++
			if capture && started.IsZero() {
				started = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return traversal, events, measuredElapsed(started), ctx.Err()
		case <-g.done[predecessor]:
		}
	}
	return traversal, events, measuredElapsed(started), nil
}

func (g *explicitDependencyGate) Complete(transaction int) {
	g.once[transaction].Do(func() { close(g.done[transaction]) })
}

type summaryDependencyGate struct {
	barriers  []int
	mu        sync.Mutex
	completed []bool
	frontier  int
	wake      chan struct{}
}

func newSummaryDependencyGate(barriers []int) *summaryDependencyGate {
	return &summaryDependencyGate{
		barriers:  barriers,
		completed: make([]bool, len(barriers)),
		wake:      make(chan struct{}),
	}
}

func (g *summaryDependencyGate) Wait(ctx context.Context, transaction int, capture bool) (uint64, uint64, uint64, error) {
	barrier := g.barriers[transaction]
	if barrier < 0 {
		return 1, 0, 0, nil
	}
	var started time.Time
	var traversal uint64
	for {
		traversal++
		g.mu.Lock()
		if g.frontier > barrier {
			g.mu.Unlock()
			return traversal, 0, measuredElapsed(started), nil
		}
		wake := g.wake
		g.mu.Unlock()
		if capture && started.IsZero() {
			started = time.Now()
		}
		select {
		case <-ctx.Done():
			return traversal, 1, measuredElapsed(started), ctx.Err()
		case <-wake:
		}
		// A summary wait is one resolution event even if several frontier
		// advances are required.
		g.mu.Lock()
		resolved := g.frontier > barrier
		g.mu.Unlock()
		if resolved {
			return traversal, 1, measuredElapsed(started), nil
		}
	}
}

func (g *summaryDependencyGate) Complete(transaction int) {
	g.mu.Lock()
	if g.completed[transaction] {
		g.mu.Unlock()
		return
	}
	g.completed[transaction] = true
	before := g.frontier
	for g.frontier < len(g.completed) && g.completed[g.frontier] {
		g.frontier++
	}
	if g.frontier != before {
		close(g.wake)
		g.wake = make(chan struct{})
	}
	g.mu.Unlock()
}
