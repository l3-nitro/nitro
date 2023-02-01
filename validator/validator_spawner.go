package validator

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/offchainlabs/nitro/util/stopwaiter"
	flag "github.com/spf13/pflag"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

type ReadyMarker interface {
	Ready() bool
	ReadyChan() chan struct{}
	WaitReady(ctx context.Context) error
}

type ValidationSpawner interface {
	Launch(entry *ValidationInput, moduleRoot common.Hash) ValidationRun
	Start(context.Context)
	Stop()
	Name() string
	Room() int
}

type ValidationRun interface {
	ReadyMarker
	WasmModuleRoot() common.Hash
	Result() (GoGlobalState, error)
	Close()
}

type ArbitratorSpawnerConfig struct {
	ConcurrentRuns     int    `koanf:"concurrent-runs-limit" reload:"hot"`
	OutputPath         string `koanf:"output-path" reload:"hot"`
	TargetMachineCount int    `koanf:"target-machine-count"`
}

type ArbitratorSpawnerConfigFecher func() *ArbitratorSpawnerConfig

var DefaultArbitratorSpawnerConfig = ArbitratorSpawnerConfig{
	ConcurrentRuns:     0,
	OutputPath:         "./target/output",
	TargetMachineCount: 4,
}

func ArbitratorSpawnerConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Int(prefix+".concurrent-runs-limit", DefaultArbitratorSpawnerConfig.ConcurrentRuns, "number of cuncurrent runs")
	f.String(prefix+".output-path", DefaultArbitratorSpawnerConfig.OutputPath, "path to write machines to")
	f.Int(prefix+".target-machine-count", DefaultArbitratorSpawnerConfig.TargetMachineCount, "target machine count")
}

func DefaultArbitratorSpawnerConfigFetcher() *ArbitratorSpawnerConfig {
	return &DefaultArbitratorSpawnerConfig
}

type JitSpawnerConfig struct {
	ConcurrentRuns int  `koanf:"concurrent-runs-limit" reload:"hot"`
	Cranelift      bool `koanf:"cranelift"`
}

type JitSpawnerConfigFecher func() *JitSpawnerConfig

var DefaultJitSpawnerConfig = JitSpawnerConfig{
	ConcurrentRuns: 0,
	Cranelift:      true,
}

func JitSpawnerConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Int(prefix+".concurrent-runs-limit", DefaultJitSpawnerConfig.ConcurrentRuns, "number of cuncurrent runs")
	f.Bool(prefix+".cranelift", DefaultJitSpawnerConfig.Cranelift, "use Cranelift instead of LLVM when validating blocks using the jit-accelerated block validator")
}

// joint for comfort only - the two configs are entirely separate.
type ValidationConfig struct {
	Arbitrator ArbitratorSpawnerConfig `koanf:"arbitrator" reload:"hot"`
	Jit        JitSpawnerConfig        `koanf:"jit" reload:"hot"`
}

var DefaultValidationConfig = ValidationConfig{
	Jit:        DefaultJitSpawnerConfig,
	Arbitrator: DefaultArbitratorSpawnerConfig,
}

func ValidationConfigAddOptions(prefix string, f *flag.FlagSet) {
	ArbitratorSpawnerConfigAddOptions(prefix+".arbitrator", f)
	JitSpawnerConfigAddOptions(prefix+".jit", f)
}

type ArbitratorSpawner struct {
	stopwaiter.StopWaiter
	count         int32
	locator       *MachineLocator
	machineLoader *ArbMachineLoader
	config        ArbitratorSpawnerConfigFecher
}

type readyMarker struct {
	chanReady chan struct{}
	boolReady int32
	err       error
}

type valRun struct {
	readyMarker
	root   common.Hash
	result GoGlobalState
}

var ErrNotReady error = errors.New("not ready")

func (d *readyMarker) Ready() bool {
	return atomic.LoadInt32(&d.boolReady) != 0
}

func (d *readyMarker) ReadyChan() chan struct{} {
	return d.chanReady
}

func (d *readyMarker) WaitReady(ctx context.Context) error {
	select {
	case <-d.chanReady:
		return d.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *readyMarker) signalReady(err error) {
	d.err = err
	atomic.StoreInt32(&d.boolReady, 1)
	close(d.chanReady)
}

func newReadyMarker() readyMarker {
	return readyMarker{
		boolReady: 0,
		chanReady: make(chan struct{}),
	}
}

func (r *valRun) Result() (GoGlobalState, error) {
	if !r.Ready() {
		return GoGlobalState{}, ErrNotReady
	}
	return r.result, r.err
}

func (r *valRun) WasmModuleRoot() common.Hash {
	return r.root
}

func (r *valRun) Close() {}

func NewvalRun(root common.Hash) *valRun {
	return &valRun{
		readyMarker: newReadyMarker(),
		root:        root,
	}
}

func (r *valRun) consumeResult(res GoGlobalState, err error) {
	r.result = res
	r.signalReady(err)
}

func NewArbitratorSpawner(locator *MachineLocator, config ArbitratorSpawnerConfigFecher) (*ArbitratorSpawner, error) {
	// TODO: preload machines
	spawner := &ArbitratorSpawner{
		locator:       locator,
		machineLoader: NewArbMachineLoader(&DefaultArbitratorMachineConfig, locator),
		config:        config,
	}
	return spawner, nil
}

func (s *ArbitratorSpawner) Start(ctx_in context.Context) {
	// could be used as both exec and validation spawner
	if !s.Started() {
		s.StopWaiter.Start(ctx_in, s)
	}
}

func (s *ArbitratorSpawner) LatestWasmModuleRoot() (common.Hash, error) {
	return s.locator.LatestWasmModuleRoot(), nil
}

func (s *ArbitratorSpawner) Name() string {
	return "arbitrator"
}

func (v *ArbitratorSpawner) loadEntryToMachine(ctx context.Context, entry *ValidationInput, mach *ArbitratorMachine) error {
	resolver := func(hash common.Hash) ([]byte, error) {
		// Check if it's a known preimage
		if preimage, ok := entry.Preimages[hash]; ok {
			return preimage, nil
		}
		return nil, errors.New("preimage not found")
	}
	if err := mach.SetPreimageResolver(resolver); err != nil {
		return err
	}
	err := mach.SetGlobalState(entry.StartState)
	if err != nil {
		log.Error("error while setting global state for proving", "err", err, "gsStart", entry.StartState)
		return fmt.Errorf("error while setting global state for proving: %w", err)
	}
	for _, batch := range entry.BatchInfo {
		err = mach.AddSequencerInboxMessage(batch.Number, batch.Data)
		if err != nil {
			log.Error(
				"error while trying to add sequencer msg for proving",
				"err", err, "seq", entry.StartState.Batch, "blockNr", entry.Id,
			)
			return fmt.Errorf("error while trying to add sequencer msg for proving: %w", err)
		}
	}
	if entry.HasDelayedMsg {
		err = mach.AddDelayedInboxMessage(entry.DelayedMsgNr, entry.DelayedMsg)
		if err != nil {
			log.Error(
				"error while trying to add delayed msg for proving",
				"err", err, "seq", entry.DelayedMsgNr, "blockNr", entry.Id,
			)
			return fmt.Errorf("error while trying to add delayed msg for proving: %w", err)
		}
	}
	return nil
}

func (v *ArbitratorSpawner) execute(
	ctx context.Context, entry *ValidationInput, moduleRoot common.Hash,
) (GoGlobalState, error) {
	basemachine, err := v.machineLoader.GetHostIoMachine(ctx, moduleRoot)
	if err != nil {
		return GoGlobalState{}, fmt.Errorf("unabled to get WASM machine: %w", err)
	}

	mach := basemachine.Clone()
	defer mach.Destroy()
	err = v.loadEntryToMachine(ctx, entry, mach)
	if err != nil {
		return GoGlobalState{}, err
	}
	var steps uint64
	for mach.IsRunning() {
		var count uint64 = 500000000
		err = mach.Step(ctx, count)
		if steps > 0 {
			log.Debug("validation", "moduleRoot", moduleRoot, "block", entry.Id, "steps", steps)
		}
		if err != nil {
			return GoGlobalState{}, fmt.Errorf("machine execution failed with error: %w", err)
		}
		steps += count
	}
	if mach.IsErrored() {
		log.Error("machine entered errored state during attempted validation", "block", entry.Id)
		return GoGlobalState{}, errors.New("machine entered errored state during attempted validation")
	}
	return mach.GetGlobalState(), nil
}

func (v *ArbitratorSpawner) Launch(entry *ValidationInput, moduleRoot common.Hash) ValidationRun {
	atomic.AddInt32(&v.count, 1)
	run := NewvalRun(moduleRoot)
	v.LaunchThread(func(ctx context.Context) {
		defer atomic.AddInt32(&v.count, -1)
		run.consumeResult(v.execute(ctx, entry, moduleRoot))
	})
	return run
}

func (v *ArbitratorSpawner) Room() int {
	avail := v.config().ConcurrentRuns
	if avail == 0 {
		avail = runtime.NumCPU()
	}
	return avail - int(atomic.LoadInt32(&v.count))
}

var launchTime = time.Now().Format("2006_01_02__15_04")

//nolint:gosec
func (v *ArbitratorSpawner) WriteToFile(input *ValidationInput, expOut GoGlobalState, moduleRoot common.Hash) error {
	outDirPath := filepath.Join(v.locator.rootPath, v.config().OutputPath, launchTime, fmt.Sprintf("block_%d", input.Id))
	err := os.MkdirAll(outDirPath, 0755)
	if err != nil {
		return err
	}

	rootPathAssign := ""
	if executable, err := os.Executable(); err == nil {
		rootPathAssign = "ROOTPATH=\"" + filepath.Dir(executable) + "\"\n"
	}
	cmdFile, err := os.OpenFile(filepath.Join(outDirPath, "run-prover.sh"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer cmdFile.Close()
	_, err = cmdFile.WriteString("#!/bin/bash\n" +
		fmt.Sprintf("# expected output: batch %d, postion %d, hash %s\n", expOut.Batch, expOut.PosInBatch, expOut.BlockHash) +
		"MACHPATH=\"" + v.locator.getMachinePath(moduleRoot) + "\"\n" +
		rootPathAssign +
		"if (( $# > 1 )); then\n" +
		"	if [[ $1 == \"-m\" ]]; then\n" +
		"		MACHPATH=$2\n" +
		"		shift\n" +
		"		shift\n" +
		"	fi\n" +
		"fi\n" +
		"${ROOTPATH}/bin/prover ${MACHPATH}/replay.wasm")
	if err != nil {
		return err
	}

	libraries := []string{"soft-float.wasm", "wasi_stub.wasm", "go_stub.wasm", "host_io.wasm", "brotli.wasm"}
	for _, module := range libraries {
		_, err = cmdFile.WriteString(" -l " + "${MACHPATH}/" + module)
		if err != nil {
			return err
		}
	}
	_, err = cmdFile.WriteString(fmt.Sprintf(" --inbox-position %d --position-within-message %d --last-block-hash %s", input.StartState.Batch, input.StartState.PosInBatch, input.StartState.BlockHash))
	if err != nil {
		return err
	}

	for _, msg := range input.BatchInfo {
		sequencerFileName := fmt.Sprintf("sequencer_%d.bin", msg.Number)
		err = os.WriteFile(filepath.Join(outDirPath, sequencerFileName), msg.Data, 0644)
		if err != nil {
			return err
		}
		_, err = cmdFile.WriteString(" --inbox " + sequencerFileName)
		if err != nil {
			return err
		}
	}

	preimageFile, err := os.Create(filepath.Join(outDirPath, "preimages.bin"))
	if err != nil {
		return err
	}
	defer preimageFile.Close()
	for _, data := range input.Preimages {
		lenbytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(lenbytes, uint64(len(data)))
		_, err := preimageFile.Write(lenbytes)
		if err != nil {
			return err
		}
		_, err = preimageFile.Write(data)
		if err != nil {
			return err
		}
	}

	_, err = cmdFile.WriteString(" --preimages preimages.bin")
	if err != nil {
		return err
	}

	if input.HasDelayedMsg {
		_, err = cmdFile.WriteString(fmt.Sprintf(" --delayed-inbox-position %d", input.DelayedMsgNr))
		if err != nil {
			return err
		}
		filename := fmt.Sprintf("delayed_%d.bin", input.DelayedMsgNr)
		err = os.WriteFile(filepath.Join(outDirPath, filename), input.DelayedMsg, 0644)
		if err != nil {
			return err
		}
		_, err = cmdFile.WriteString(fmt.Sprintf(" --delayed-inbox %s", filename))
		if err != nil {
			return err
		}
	}

	_, err = cmdFile.WriteString(" \"$@\"\n")
	if err != nil {
		return err
	}
	return nil
}

func (v *ArbitratorSpawner) CreateExecutionBackend(ctx context.Context, wasmModuleRoot common.Hash, input *ValidationInput) (*ExecutionChallengeBackend, error) {
	initialFrozenMachine, err := v.machineLoader.GetZeroStepMachine(ctx, wasmModuleRoot)
	if err != nil {
		return nil, err
	}
	machine := initialFrozenMachine.Clone()
	err = v.loadEntryToMachine(ctx, input, machine)
	if err != nil {
		return nil, err
	}
	machine.Freeze()
	return NewExecutionChallengeBackend(machine, v.config().TargetMachineCount, nil)
}

func (v *ArbitratorSpawner) Stop() {
	v.StopOnly()
}

type JitSpawner struct {
	stopwaiter.StopWaiter
	count         int32
	locator       *MachineLocator
	machineLoader *JitMachineLoader
	config        JitSpawnerConfigFecher
}

func NewJitSpawner(locator *MachineLocator, config JitSpawnerConfigFecher, fatalErrChan chan error) (*JitSpawner, error) {
	// TODO - preload machines
	machineConfig := DefaultJitMachineConfig
	machineConfig.JitCranelift = config().Cranelift
	loader, err := NewJitMachineLoader(&machineConfig, locator, fatalErrChan)
	if err != nil {
		return nil, err
	}
	spawner := &JitSpawner{
		locator:       locator,
		machineLoader: loader,
		config:        config,
	}
	return spawner, nil
}

func (v *JitSpawner) Start(ctx_in context.Context) {
	v.StopWaiter.Start(ctx_in, v)
}

func (v *JitSpawner) execute(
	ctx context.Context, entry *ValidationInput, moduleRoot common.Hash,
) (GoGlobalState, error) {
	empty := GoGlobalState{}

	machine, err := v.machineLoader.GetMachine(ctx, moduleRoot)
	if err != nil {
		return empty, fmt.Errorf("unabled to get WASM machine: %w", err)
	}

	resolver := func(hash common.Hash) ([]byte, error) {
		// Check if it's a known preimage
		if preimage, ok := entry.Preimages[hash]; ok {
			return preimage, nil
		}
		return nil, errors.New("preimage not found")
	}
	state, err := machine.prove(ctx, entry, resolver)
	return state, err
}

func (s *JitSpawner) Name() string {
	if s.config().Cranelift {
		return "jit-cranelift"
	}
	return "jit"
}

func (v *JitSpawner) Launch(entry *ValidationInput, moduleRoot common.Hash) ValidationRun {
	atomic.AddInt32(&v.count, 1)
	run := NewvalRun(moduleRoot)
	go func() {
		defer atomic.AddInt32(&v.count, -1)
		run.consumeResult(v.execute(v.GetContext(), entry, moduleRoot))
	}()
	return run
}

func (v *JitSpawner) Room() int {
	avail := v.config().ConcurrentRuns
	if avail == 0 {
		avail = runtime.NumCPU()
	}
	return avail - int(atomic.LoadInt32(&v.count))
}

func (v *JitSpawner) Stop() {
	v.StopOnly()
	v.machineLoader.Stop()
}
