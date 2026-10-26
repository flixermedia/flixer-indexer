package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

const (
	maxModuleBytes = 16 << 20
	maxInputBytes  = 1 << 20
	maxOutputBytes = 1 << 20
	moduleMemory   = 256
	maxCompiled    = 256
	defaultTimeout = 3 * time.Second
)

// Runner executes WASI modules without preopened files, environment variables, or host network imports.
type Runner struct {
	runtime  wazero.Runtime
	mu       sync.Mutex
	compiled map[[sha256.Size]byte]wazero.CompiledModule
}

func New(ctx context.Context) (*Runner, error) {
	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithMemoryLimitPages(moduleMemory).WithCloseOnContextDone(true))
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
		_ = runtime.Close(ctx)
		return nil, fmt.Errorf("initialize WASI runtime: %w", err)
	}
	return &Runner{runtime: runtime, compiled: map[[sha256.Size]byte]wazero.CompiledModule{}}, nil
}

func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, module := range r.compiled {
		_ = module.Close(ctx)
	}
	r.compiled = map[[sha256.Size]byte]wazero.CompiledModule{}
	return r.runtime.Close(ctx)
}

func (r *Runner) Run(ctx context.Context, modulePath string, input []byte) ([]byte, error) {
	if len(input) > maxInputBytes {
		return nil, errors.New("plugin request exceeds the input size limit")
	}
	moduleBytes, err := os.ReadFile(modulePath)
	if err != nil {
		return nil, fmt.Errorf("read plugin module: %w", err)
	}
	if len(moduleBytes) == 0 || len(moduleBytes) > maxModuleBytes {
		return nil, errors.New("plugin module has an invalid size")
	}

	callCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	digest := sha256.Sum256(moduleBytes)
	compiled, err := r.compile(callCtx, digest, moduleBytes)
	if err != nil {
		return nil, fmt.Errorf("compile plugin module: %w", err)
	}
	stdout := &limitedBuffer{limit: maxOutputBytes}
	module, err := r.runtime.InstantiateModule(callCtx, compiled, wazero.NewModuleConfig().WithStdin(bytes.NewReader(input)).WithStdout(stdout).WithStderr(io.Discard).WithArgs("flixer-plugin").WithStartFunctions())
	if err != nil {
		return nil, fmt.Errorf("instantiate plugin module: %w", err)
	}
	defer module.Close(context.Background())
	start := module.ExportedFunction("_start")
	if start == nil {
		return nil, errors.New("plugin module does not export the WASI _start function")
	}
	if _, err := start.Call(callCtx); err != nil {
		var exitErr *sys.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 0 {
			return nil, fmt.Errorf("execute plugin module: %w", err)
		}
	}
	return stdout.Bytes(), nil
}

// Validate checks a module during activation without invoking its command entry point.
func (r *Runner) Validate(ctx context.Context, modulePath string) error {
	moduleBytes, err := os.ReadFile(modulePath)
	if err != nil {
		return fmt.Errorf("read plugin module: %w", err)
	}
	if len(moduleBytes) == 0 || len(moduleBytes) > maxModuleBytes {
		return errors.New("plugin module has an invalid size")
	}
	validateCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	digest := sha256.Sum256(moduleBytes)
	_, err = r.compile(validateCtx, digest, moduleBytes)
	return err
}

func (r *Runner) compile(ctx context.Context, digest [sha256.Size]byte, moduleBytes []byte) (wazero.CompiledModule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if compiled := r.compiled[digest]; compiled != nil {
		return compiled, nil
	}
	if len(r.compiled) >= maxCompiled {
		return nil, errors.New("plugin module cache is full")
	}
	compiled, err := r.runtime.CompileModule(ctx, moduleBytes)
	if err != nil {
		return nil, err
	}
	r.compiled[digest] = compiled
	return compiled, nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if b.Len()+len(data) > b.limit {
		return 0, errors.New("plugin output exceeds the size limit")
	}
	return b.Buffer.Write(data)
}
